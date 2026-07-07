//go:build windows

package main

import "encoding/binary"

func init() {
	iconOn = windowsICOFromPNG(iconOn)
	iconOff = windowsICOFromPNG(iconOff)
}

func windowsICOFromPNG(png []byte) []byte {
	// ICO container with one PNG-encoded image. Windows Vista and newer support
	// PNG payloads inside .ico files, and fyne.io/systray expects ICO bytes on
	// Windows.
	const headerSize = 6
	const dirEntrySize = 16
	const imageOffset = headerSize + dirEntrySize
	ico := make([]byte, imageOffset+len(png))
	binary.LittleEndian.PutUint16(ico[2:4], 1) // type: icon
	binary.LittleEndian.PutUint16(ico[4:6], 1) // one image
	ico[6] = 22                                // width; 0 means 256, so keep icon's actual 22px
	ico[7] = 22                                // height
	ico[8] = 0                                 // color count
	ico[9] = 0                                 // reserved
	binary.LittleEndian.PutUint16(ico[10:12], 1)
	binary.LittleEndian.PutUint16(ico[12:14], 32)
	binary.LittleEndian.PutUint32(ico[14:18], uint32(len(png)))
	binary.LittleEndian.PutUint32(ico[18:22], imageOffset)
	copy(ico[imageOffset:], png)
	return ico
}
