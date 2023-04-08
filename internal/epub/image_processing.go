package epub

import (
	"archive/zip"
	"bytes"
	"fmt"
	"image"
	"image/color"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/celogeek/go-comic-converter/v2/internal/epub/sortpath"
	"github.com/disintegration/gift"
	"github.com/nwaples/rardecode"
	pdfimage "github.com/raff/pdfreader/image"
	"github.com/raff/pdfreader/pdfread"
	"golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"
)

type Image struct {
	Id        int
	Part      int
	Data      *ImageData
	Width     int
	Height    int
	IsCover   bool
	NeedSpace bool
	Path      string
}

type imageTask struct {
	Id       int
	Reader   io.ReadCloser
	Path     string
	Filename string
}

func colorIsBlank(c color.Color) bool {
	g := color.GrayModel.Convert(c).(color.Gray)
	return g.Y >= 0xf0
}

func findMarging(img image.Image) image.Rectangle {
	imgArea := img.Bounds()

LEFT:
	for x := imgArea.Min.X; x < imgArea.Max.X; x++ {
		for y := imgArea.Min.Y; y < imgArea.Max.Y; y++ {
			if !colorIsBlank(img.At(x, y)) {
				break LEFT
			}
		}
		imgArea.Min.X++
	}

UP:
	for y := imgArea.Min.Y; y < imgArea.Max.Y; y++ {
		for x := imgArea.Min.X; x < imgArea.Max.X; x++ {
			if !colorIsBlank(img.At(x, y)) {
				break UP
			}
		}
		imgArea.Min.Y++
	}

RIGHT:
	for x := imgArea.Max.X - 1; x >= imgArea.Min.X; x-- {
		for y := imgArea.Min.Y; y < imgArea.Max.Y; y++ {
			if !colorIsBlank(img.At(x, y)) {
				break RIGHT
			}
		}
		imgArea.Max.X--
	}

BOTTOM:
	for y := imgArea.Max.Y - 1; y >= imgArea.Min.Y; y-- {
		for x := imgArea.Min.X; x < imgArea.Max.X; x++ {
			if !colorIsBlank(img.At(x, y)) {
				break BOTTOM
			}
		}
		imgArea.Max.Y--
	}

	return imgArea
}

func LoadImages(path string, options *ImageOptions, dry bool) ([]*Image, error) {
	images := make([]*Image, 0)

	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}

	var (
		imageCount int
		imageInput chan *imageTask
	)

	if fi.IsDir() {
		imageCount, imageInput, err = loadDir(path)
	} else {
		switch ext := strings.ToLower(filepath.Ext(path)); ext {
		case ".cbz", ".zip":
			imageCount, imageInput, err = loadCbz(path)
		case ".cbr", ".rar":
			imageCount, imageInput, err = loadCbr(path)
		case ".pdf":
			imageCount, imageInput, err = loadPdf(path)
		default:
			err = fmt.Errorf("unknown file format (%s): support .cbz, .zip, .cbr, .rar, .pdf", ext)
		}
	}
	if err != nil {
		return nil, err
	}

	if dry {
		for img := range imageInput {
			images = append(images, &Image{
				img.Id,
				0,
				nil,
				0,
				0,
				false,
				false, // NeedSpace reajust during parts computation
				img.Path,
			})
		}

		return images, nil
	}

	imageOutput := make(chan *Image)

	// processing
	bar := NewBar(imageCount, "Processing", 1, 2)
	wg := &sync.WaitGroup{}

	for i := 0; i < options.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			for img := range imageInput {
				// Decode image
				src, _, err := image.Decode(img.Reader)
				if err != nil {
					bar.Clear()
					fmt.Fprintf(os.Stderr, "error processing image %s: %s\n", img.Filename, err)
					os.Exit(1)
				}

				if options.Crop {
					g := gift.New(gift.Crop(findMarging(src)))
					newSrc := image.NewNRGBA(g.Bounds(src.Bounds()))
					g.Draw(newSrc, src)
					src = newSrc
				}

				g := NewGift(options)

				// Convert image
				dst := image.NewPaletted(g.Bounds(src.Bounds()), options.Palette)
				g.Draw(dst, src)

				imageOutput <- &Image{
					img.Id,
					0,
					newImageData(img.Id, 0, dst, options.Quality),
					dst.Bounds().Dx(),
					dst.Bounds().Dy(),
					img.Id == 0,
					false,
					img.Path,
				}

				// Auto split double page
				// Except for cover
				// Only if the src image have width > height and is bigger than the view
				if (!options.HasCover || img.Id > 0) &&
					options.AutoSplitDoublePage &&
					src.Bounds().Dx() > src.Bounds().Dy() &&
					src.Bounds().Dx() > options.ViewHeight &&
					src.Bounds().Dy() > options.ViewWidth {
					gifts := NewGiftSplitDoublePage(options)
					for i, g := range gifts {
						part := i + 1
						dst := image.NewPaletted(g.Bounds(src.Bounds()), options.Palette)
						g.Draw(dst, src)
						imageOutput <- &Image{
							img.Id,
							part,
							newImageData(img.Id, part, dst, options.Quality),
							dst.Bounds().Dx(),
							dst.Bounds().Dy(),
							false,
							false, // NeedSpace reajust during parts computation
							img.Path,
						}
					}
				}
			}
		}()
	}

	go func() {
		wg.Wait()
		close(imageOutput)
	}()

	for image := range imageOutput {
		if !(options.NoBlankPage && image.Width == 1 && image.Height == 1) {
			images = append(images, image)
		}
		if image.Part == 0 {
			bar.Add(1)
		}
	}
	bar.Close()

	if len(images) == 0 {
		return nil, fmt.Errorf("image not found")
	}

	return images, nil
}

func isSupportedImage(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jpg", ".jpeg", ".png", ".webp":
		{
			return true
		}
	}
	return false
}

func loadDir(input string) (int, chan *imageTask, error) {
	images := make([]string, 0)
	input = filepath.Clean(input)
	err := filepath.WalkDir(input, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && isSupportedImage(path) {
			images = append(images, path)
		}
		return nil
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if len(images) == 0 {
		return 0, nil, fmt.Errorf("image not found")
	}

	sort.Sort(sortpath.By(images))

	output := make(chan *imageTask)
	go func() {
		defer close(output)
		for i, img := range images {
			f, err := os.Open(img)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			p := filepath.Dir(img)
			if p == input {
				p = ""
			} else {
				p = p[len(input)+1:]
			}
			output <- &imageTask{
				Id:       i,
				Reader:   f,
				Path:     p,
				Filename: img,
			}
		}
	}()
	return len(images), output, nil
}

func loadCbz(input string) (int, chan *imageTask, error) {
	r, err := zip.OpenReader(input)
	if err != nil {
		return 0, nil, err
	}

	images := make([]*zip.File, 0)
	for _, f := range r.File {
		if !f.FileInfo().IsDir() && isSupportedImage(f.Name) {
			images = append(images, f)
		}
	}
	if len(images) == 0 {
		r.Close()
		return 0, nil, fmt.Errorf("no images found")
	}

	names := []string{}
	for _, img := range images {
		names = append(names, img.Name)
	}
	sort.Sort(sortpath.By(names))

	indexedNames := make(map[string]int)
	for i, name := range names {
		indexedNames[name] = i
	}

	output := make(chan *imageTask)
	go func() {
		defer close(output)
		for _, img := range images {
			f, err := img.Open()
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			output <- &imageTask{
				Id:       indexedNames[img.Name],
				Reader:   f,
				Path:     filepath.Dir(filepath.Clean(img.Name)),
				Filename: img.Name,
			}
		}
	}()
	return len(images), output, nil
}

func loadCbr(input string) (int, chan *imageTask, error) {
	// listing and indexing
	rl, err := rardecode.OpenReader(input, "")
	if err != nil {
		return 0, nil, err
	}
	names := make([]string, 0)
	for {
		f, err := rl.Next()

		if err != nil && err != io.EOF {
			rl.Close()
			return 0, nil, err
		}

		if f == nil {
			break
		}

		if !f.IsDir && isSupportedImage(f.Name) {
			names = append(names, f.Name)
		}
	}
	rl.Close()

	if len(names) == 0 {
		return 0, nil, fmt.Errorf("no images found")
	}

	sort.Sort(sortpath.By(names))

	indexedNames := make(map[string]int)
	for i, name := range names {
		indexedNames[name] = i
	}

	// send file to the queue
	output := make(chan *imageTask)
	go func() {
		defer close(output)
		r, err := rardecode.OpenReader(input, "")
		if err != nil {
			panic(err)
		}
		defer r.Close()

		for {
			f, err := r.Next()
			if err != nil && err != io.EOF {
				panic(err)
			}
			if f == nil {
				break
			}
			if idx, ok := indexedNames[f.Name]; ok {
				b := bytes.NewBuffer([]byte{})
				io.Copy(b, r)

				output <- &imageTask{
					Id:       idx,
					Reader:   io.NopCloser(b),
					Path:     filepath.Dir(filepath.Clean(f.Name)),
					Filename: f.Name,
				}
			}
		}
	}()

	return len(names), output, nil
}

func loadPdf(input string) (int, chan *imageTask, error) {
	pdf := pdfread.Load(input)
	if pdf == nil {
		return 0, nil, fmt.Errorf("can't read pdf")
	}

	nbPages := len(pdf.Pages())
	output := make(chan *imageTask)
	go func() {
		defer close(output)
		defer pdf.Close()
		for i := 0; i < nbPages; i++ {
			img, err := pdfimage.Extract(pdf, i+1)
			if err != nil {
				panic(err)
			}

			b := bytes.NewBuffer([]byte{})
			err = tiff.Encode(b, img, nil)
			if err != nil {
				panic(err)
			}

			output <- &imageTask{
				Id:       i,
				Reader:   io.NopCloser(b),
				Path:     "/",
				Filename: fmt.Sprintf("page %d", i+1),
			}
		}
	}()

	return nbPages, output, nil
}
