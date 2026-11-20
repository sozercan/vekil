package main

/*
#cgo CFLAGS: -x objective-c -fobjc-arc
#cgo LDFLAGS: -framework Cocoa
#include "tray_icon_bridge.h"
*/
import "C"

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"sync"
	"unsafe"

	"fyne.io/systray"
)

// use a slightly larger image than systray's default so the robot's padded
// artwork has the same visible weight as neighboring menu bar icons.
const (
	trayImagePoints          = C.VEKIL_TRAY_IMAGE_POINTS
	trayBadgeCenterX         = C.VEKIL_TRAY_BADGE_CENTER_X
	trayBadgeCenterTop       = trayImagePoints - C.VEKIL_TRAY_BADGE_CENTER_Y
	trayBadgeClearanceRadius = C.VEKIL_TRAY_BADGE_CLEARANCE_RADIUS
)

type trayTemplateImages struct {
	plain   []byte
	stopped []byte
	running []byte
}

var loadTrayTemplates = sync.OnceValues(func() (trayTemplateImages, error) {
	var templates trayTemplateImages
	var err error
	if templates.plain, err = makeTrayTemplate(255, false); err != nil {
		return templates, err
	}
	if templates.stopped, err = makeTrayTemplate(166, false); err != nil {
		return templates, err
	}
	if templates.running, err = makeTrayTemplate(255, true); err != nil {
		return templates, err
	}
	return templates, nil
})

func makeTrayTemplate(opacity uint8, badge bool) ([]byte, error) {
	source, err := png.Decode(bytes.NewReader(iconOn))
	if err != nil {
		return nil, fmt.Errorf("decode robot icon: %w", err)
	}
	// preserve the original pixel artwork at twice its source resolution, with
	// a supersampled clearance edge for the vector badge.
	size := source.Bounds().Dx() * 2
	mask := image.NewNRGBA(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			_, _, _, alpha := source.At(x/2, y/2).RGBA()
			coverage := 16
			if badge {
				for sampleY := 0; sampleY < 4; sampleY++ {
					for sampleX := 0; sampleX < 4; sampleX++ {
						pointX := (float64(x) + (float64(sampleX)+0.5)/4) * trayImagePoints / float64(size)
						pointY := (float64(y) + (float64(sampleY)+0.5)/4) * trayImagePoints / float64(size)
						if (pointX-trayBadgeCenterX)*(pointX-trayBadgeCenterX)+(pointY-trayBadgeCenterTop)*(pointY-trayBadgeCenterTop) < trayBadgeClearanceRadius*trayBadgeClearanceRadius {
							coverage--
						}
					}
				}
			}
			mask.SetNRGBA(x, y, color.NRGBA{A: uint8(uint64(alpha) * uint64(opacity) * uint64(coverage) / (65535 * 16))})
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
	fallback := templates.plain
	if status == trayStopped {
		fallback = templates.stopped
	}
	icon := fallback
	if status == trayRunning {
		icon = templates.running
	}
	message := C.vekil_tray_set_icon(unsafe.Pointer(&icon[0]), C.int(len(icon)), C.bool(status == trayRunning))
	if message != nil {
		// keep a complete, adaptive robot if systray's private integration ever
		// changes. Never show the badge cutout without its colored overlay.
		systray.SetTemplateIcon(fallback, iconOff)
		return fmt.Errorf("attach native tray icon: %s", C.GoString(message))
	}
	return nil
}
