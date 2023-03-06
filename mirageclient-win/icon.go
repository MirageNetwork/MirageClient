//go:build windows
package main

import (
	"embed"
	"image/png"
	"log"

	"github.com/tailscale/walk"
)

//go:embed logos
var iconDir embed.FS

var Icons []*walk.Icon

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

var iconName = map[IconType]string{
	Logo:     "logo",
	Disconn:  "disconn",
	Conn:     "conn",
	Exit:     "exit",
	AsExit:   "asexit",
	HasIssue: "issue",
	Ing1:     "ing1",
	Ing2:     "ing2"}

func (ico IconType) String() string {
	return iconName[ico]
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
	Icons = make([]*walk.Icon, len(iconName))
	for i := range iconName {
		Icons[i], err = i.load()
		if err != nil {
			log.Fatal(err)
		}
	}
}
