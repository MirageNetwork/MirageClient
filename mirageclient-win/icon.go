package main

import (
	"embed"
	"image/png"
	"log"

	"github.com/tailscale/walk"
)

//go:embed logos
var iconDir embed.FS

var icons []*walk.Icon

type IconType int

const (
	Logo IconType = iota
	Disconn
	Conn
	Exit
	AsExit
	HasIssue
	Ing1
	Ing2
)

var IconTypes = map[IconType]string{
	Logo:     "logo",
	Disconn:  "disconn",
	Conn:     "conn",
	Exit:     "exit",
	AsExit:   "asexit",
	HasIssue: "issue",
	Ing1:     "ing1",
	Ing2:     "ing2"}

func (ico IconType) String() string {
	return IconTypes[ico]
}

func (ico IconType) load() (*walk.Icon, error) {
	file, err := iconDir.Open("logos/" + ico.String() + ".png")
	defer file.Close()
	if err != nil {
		log.Fatal(err)
		return nil, err
	}
	iconImage, err := png.Decode(file)
	if err != nil {
		log.Fatal(err)
		return nil, err
	}
	icon, err := walk.NewIconFromImage(iconImage)
	return icon, err
}

func init() {
	var err error
	icons = make([]*walk.Icon, len(IconTypes))
	for i := range IconTypes {
		icons[i], err = i.load()
		if err != nil {
			log.Fatal(err)
		}
	}
}
