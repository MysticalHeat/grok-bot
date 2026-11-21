package main

import (
	"bytes"
	"encoding/binary"
	"image"
	_ "image/png"
)

const trayUploadBytes = 8 << 20

func fitTrayImage(img image.Image, maxSide int) image.Image {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if max(w, h) <= maxSide {
		return img
	}
	d := max(w, h)
	return sampleTrayImage(img, max(1, w*maxSide/d), max(1, h*maxSide/d), 1)
}

// Normalize EXIF orientation before saving; all subsequent crops use these
// displayed coordinates, never the input file's camera coordinates.
func normalizeTrayImage(img image.Image, orientation int) image.Image {
	if orientation == 1 {
		return fitTrayImage(img, 2048)
	}
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	if orientation >= 5 {
		w, h = h, w
	}
	d := max(w, h, 2048)
	return sampleTrayImage(img, max(1, w*2048/d), max(1, h*2048/d), orientation)
}

func sampleTrayImage(img image.Image, w, h, orientation int) image.Image {
	b := img.Bounds()
	sw, sh := b.Dx(), b.Dy()
	ow, oh := sw, sh
	if orientation >= 5 {
		ow, oh = sh, sw
	}
	out := image.NewRGBA(image.Rect(0, 0, w, h))
	for j := 0; j < h; j++ {
		for i := 0; i < w; i++ {
			x, y := i*ow/w, j*oh/h
			switch orientation {
			case 2:
				x = sw - 1 - x
			case 3:
				x, y = sw-1-x, sh-1-y
			case 4:
				y = sh - 1 - y
			case 5:
				x, y = y, x
			case 6:
				x, y = y, sh-1-x
			case 7:
				x, y = sw-1-y, sh-1-x
			case 8:
				x, y = sw-1-y, x
			}
			out.Set(i, j, img.At(b.Min.X+x, b.Min.Y+y))
		}
	}
	return out
}

func trayOrientation(data []byte, format string) int {
	if format == "png" {
		for data = data[8:]; len(data) >= 12; {
			n := int64(binary.BigEndian.Uint32(data))
			if n > int64(len(data)-12) {
				break
			}
			if string(data[4:8]) == "eXIf" {
				return trayTIFFOrientation(data[8 : 8+n])
			}
			data = data[12+n:]
		}
	}
	if format == "jpeg" {
		for data = data[2:]; len(data) >= 4 && data[0] == 0xff; {
			if data[1] == 0xff {
				data = data[1:]
				continue
			}
			if data[1] == 0xda || data[1] == 0xd9 {
				break
			}
			n := int(binary.BigEndian.Uint16(data[2:]))
			if n < 2 || n > len(data)-2 {
				break
			}
			if data[1] == 0xe1 && bytes.HasPrefix(data[4:n+2], []byte("Exif\x00\x00")) {
				return trayTIFFOrientation(data[10 : n+2])
			}
			data = data[n+2:]
		}
	}
	return 1
}

func trayTIFFOrientation(b []byte) int {
	if len(b) < 8 {
		return 1
	}
	var order binary.ByteOrder
	switch string(b[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return 1
	}
	offset := int64(order.Uint32(b[4:]))
	if order.Uint16(b[2:]) != 42 || offset < 8 || offset > int64(len(b)-2) {
		return 1
	}
	b = b[offset:]
	n := int(order.Uint16(b))
	for b = b[2:]; n > 0 && len(b) >= 12; n, b = n-1, b[12:] {
		if order.Uint16(b) == 0x112 && order.Uint16(b[2:]) == 3 && order.Uint32(b[4:]) == 1 {
			v := int(order.Uint16(b[8:]))
			if v >= 1 && v <= 8 {
				return v
			}
		}
	}
	return 1
}
