package main

import (
	"flag"
	"fmt"
	"go-comic-converter/internal/epub"
	"path/filepath"
	"strings"
)

type Profile struct {
	Description string
	Width       int
	Height      int
}

var Profiles = map[string]Profile{
	"KS": {"Kindle Scribe", 1860, 2480},
}

type Option struct {
	Input   string
	Output  string
	Profile string
	Author  string
	Title   string
	Quality int
	NoCrop  bool
}

func (o *Option) String() string {
	var desc string
	var width, height int
	if profile, ok := Profiles[o.Profile]; ok {
		desc = profile.Description
		width = profile.Width
		height = profile.Height
	}

	return fmt.Sprintf(`Options:
	Input  : %s
	Output : %s
	Profile: %s - %s - %dx%d
	Author : %s
	Title  : %s
	Quality: %d
	Crop   : %v
`,
		o.Input,
		o.Output,
		o.Profile,
		desc,
		width,
		height,
		o.Author,
		o.Title,
		o.Quality,
		!o.NoCrop,
	)
}

func main() {
	availableProfiles := make([]string, 0)
	for k := range Profiles {
		availableProfiles = append(availableProfiles, k)
	}

	opt := &Option{}
	flag.StringVar(&opt.Input, "input", "", "Source of comic to convert")
	flag.StringVar(&opt.Output, "output", "", "Output of the epub")
	flag.StringVar(&opt.Profile, "profile", "", fmt.Sprintf("Profile to use: %s", strings.Join(availableProfiles, ", ")))
	flag.StringVar(&opt.Author, "author", "GO Comic Converter", "Author of the epub")
	flag.StringVar(&opt.Title, "title", "", "Title of the epub")
	flag.IntVar(&opt.Quality, "quality", 85, "Quality of the image: Default 75")
	flag.BoolVar(&opt.NoCrop, "nocrop", false, "Disable cropping: Default false")
	flag.Parse()

	if opt.Input == "" || opt.Output == "" {
		fmt.Println("Missing input or output!")
		flag.Usage()
		return
	}
	profile, profileMatch := Profiles[opt.Profile]
	if !profileMatch {
		fmt.Println("Profile doesn't exists!")
		flag.Usage()
		return
	}

	if opt.Title == "" {
		opt.Title = filepath.Base(opt.Input)
	}

	fmt.Println(opt)

	err := epub.NewEpub(opt.Output).
		SetSize(profile.Width, profile.Height).
		SetQuality(opt.Quality).
		SetCrop(!opt.NoCrop).
		SetTitle(opt.Title).
		SetAuthor(opt.Author).
		LoadDir(opt.Input).
		Write()

	if err != nil {
		panic(err)
	}
}
