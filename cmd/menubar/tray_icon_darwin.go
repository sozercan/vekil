package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"sync"

	"fyne.io/systray"
)

type trayTemplateImages struct {
	plain   []byte
	stopped []byte
}

var loadTrayTemplates = sync.OnceValues(func() (trayTemplateImages, error) {
	var templates trayTemplateImages
	source, err := png.Decode(bytes.NewReader(iconOn))
	if err != nil {
		return templates, fmt.Errorf("decode robot icon: %w", err)
	}
	if templates.plain, err = makeTrayTemplate(source, 255); err != nil {
		return templates, err
	}
	templates.stopped, err = makeTrayTemplate(source, 166)
	return templates, err
})

func makeTrayTemplate(source image.Image, opacity uint8) ([]byte, error) {
	size := source.Bounds().Dx() * 2
	mask := image.NewNRGBA(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			_, _, _, alpha := source.At(x/2, y/2).RGBA()
			mask.SetNRGBA(x, y, color.NRGBA{A: uint8(alpha * uint32(opacity) / 65535)})
		}
	}
	var output bytes.Buffer
	if err := png.Encode(&output, mask); err != nil {
		return nil, fmt.Errorf("encode robot template: %w", err)
	}
	return output.Bytes(), nil
}

func renderTrayStatus(status trayStatus) error {
	templates, err := loadTrayTemplates()
	if err != nil {
		systray.SetTemplateIcon(iconOn, iconOff)
		return err
	}
	icon := templates.plain
	if status == trayStopped {
		icon = templates.stopped
	}
	systray.SetTemplateIcon(icon, iconOff)
	return nil
}
