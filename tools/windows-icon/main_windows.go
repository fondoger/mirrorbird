package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"image/png"
	"os"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

func main() {
	exe := flag.String("exe", "", "newly built, unsigned Windows executable")
	icon := flag.String("icon", "", "PNG-based ICO file")
	flag.Parse()
	if *exe == "" || *icon == "" || flag.NArg() != 0 {
		flag.Usage()
		os.Exit(2)
	}
	data, err := os.ReadFile(*icon)
	if err == nil {
		err = applyIcon(*exe, data)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "embed Windows icon:", err)
		os.Exit(1)
	}
}

func iconResources(data []byte) ([]byte, [][]byte, error) {
	if len(data) < 6 || binary.LittleEndian.Uint16(data) != 0 || binary.LittleEndian.Uint16(data[2:]) != 1 {
		return nil, nil, errors.New("invalid ICO header")
	}
	count := int(binary.LittleEndian.Uint16(data[4:]))
	if count == 0 || len(data) < 6+16*count {
		return nil, nil, errors.New("invalid ICO directory")
	}
	group := make([]byte, 6+14*count)
	copy(group, data[:6])
	images := make([][]byte, count)
	for i := range images {
		entry := data[6+16*i : 6+16*(i+1)]
		size := uint64(binary.LittleEndian.Uint32(entry[8:]))
		offset := uint64(binary.LittleEndian.Uint32(entry[12:]))
		if size == 0 || offset < uint64(6+16*count) || offset+size > uint64(len(data)) {
			return nil, nil, fmt.Errorf("invalid ICO image %d bounds", i)
		}
		images[i] = data[offset : offset+size]
		config, err := png.DecodeConfig(bytes.NewReader(images[i]))
		width, height := int(entry[0]), int(entry[1])
		if width == 0 {
			width = 256
		}
		if height == 0 {
			height = 256
		}
		if err != nil || config.Width != width || config.Height != height {
			return nil, nil, fmt.Errorf("ICO image %d must be a %dx%d PNG", i, width, height)
		}
		copy(group[6+14*i:], entry[:12])
		binary.LittleEndian.PutUint16(group[6+14*i+12:], uint16(i+1))
	}
	return group, images, nil
}

// Use the Windows resource API after the local Go build, without leaving a
// .syso that would override MyGo's release manifest and version resources.
func applyIcon(exe string, data []byte) (err error) {
	group, images, err := iconResources(data)
	if err != nil {
		return err
	}
	filename, err := windows.UTF16PtrFromString(exe)
	if err != nil {
		return err
	}
	kernel := windows.NewLazySystemDLL("kernel32.dll")
	begin := kernel.NewProc("BeginUpdateResourceW")
	update := kernel.NewProc("UpdateResourceW")
	end := kernel.NewProc("EndUpdateResourceW")
	handle, _, callErr := begin.Call(uintptr(unsafe.Pointer(filename)), 0)
	runtime.KeepAlive(filename)
	if handle == 0 {
		return fmt.Errorf("open executable resources: %w", callErr)
	}
	discard := uintptr(1)
	defer func() {
		if ok, _, callErr := end.Call(handle, discard); ok == 0 {
			err = errors.Join(err, fmt.Errorf("finish executable resources: %w", callErr))
		}
	}()
	put := func(kind, id uintptr, contents []byte) error {
		ok, _, callErr := update.Call(handle, kind, id, 0, uintptr(unsafe.Pointer(&contents[0])), uintptr(len(contents)))
		runtime.KeepAlive(contents)
		if ok == 0 {
			return fmt.Errorf("write resource %d/%d: %w", kind, id, callErr)
		}
		return nil
	}
	for i, image := range images {
		if err := put(3, uintptr(i+1), image); err != nil { // RT_ICON
			return err
		}
	}
	if err := put(14, 1, group); err != nil { // RT_GROUP_ICON
		return err
	}
	discard = 0
	return nil
}
