package main

import (
	"bytes"
	"encoding/binary"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func readIcon(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "internal", "desktop", "app-icon.ico"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestIconResources(t *testing.T) {
	data := readIcon(t)
	group, images, err := iconResources(data)
	if err != nil {
		t.Fatal(err)
	}
	sizes := []int{16, 24, 32, 48, 64, 128, 256}
	if len(images) != len(sizes) || len(group) != 6+14*len(sizes) {
		t.Fatalf("unexpected icon directory: %d images, %d bytes", len(images), len(group))
	}
	for i, image := range images {
		config, err := png.DecodeConfig(bytes.NewReader(image))
		if err != nil || config.Width != sizes[i] || config.Height != sizes[i] {
			t.Fatalf("invalid %dpx image: %+v %v", sizes[i], config, err)
		}
		if id := binary.LittleEndian.Uint16(group[6+14*i+12:]); id != uint16(i+1) {
			t.Fatalf("invalid image ID: %d", id)
		}
	}
	for _, tc := range []struct {
		name string
		edit func([]byte) []byte
	}{
		{"short header", func(b []byte) []byte { return b[:3] }},
		{"cursor", func(b []byte) []byte { b[2] = 2; return b }},
		{"empty directory", func(b []byte) []byte { b[4], b[5] = 0, 0; return b }},
		{"short directory", func(b []byte) []byte { return b[:10] }},
		{"out of bounds", func(b []byte) []byte { binary.LittleEndian.PutUint32(b[18:], ^uint32(0)); return b }},
		{"header overlap", func(b []byte) []byte { binary.LittleEndian.PutUint32(b[18:], 0); return b }},
		{"wrong dimensions", func(b []byte) []byte { b[6]++; return b }},
		{"invalid png", func(b []byte) []byte { b[binary.LittleEndian.Uint32(b[18:])] = 0; return b }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := iconResources(tc.edit(bytes.Clone(data))); err == nil {
				t.Fatal("invalid ICO accepted")
			}
		})
	}
}

func TestApplyIcon(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	binaryData, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(t.TempDir(), "MirrorBird icon test.exe")
	if err := os.WriteFile(exe, binaryData, 0700); err != nil {
		t.Fatal(err)
	}
	icon := readIcon(t)
	for range 2 {
		if err := applyIcon(exe, icon); err != nil {
			t.Fatal(err)
		}
	}
	// Loading as data does not execute the binary or start any UI.
	module, err := windows.LoadLibraryEx(exe, 0, windows.LOAD_LIBRARY_AS_DATAFILE)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.FreeLibrary(module)
	group, images, err := iconResources(icon)
	if err != nil {
		t.Fatal(err)
	}
	check := func(kind, id uint16, want []byte) {
		t.Helper()
		resource, err := windows.FindResource(module, windows.ResourceID(id), windows.ResourceID(kind))
		if err != nil {
			t.Fatal(err)
		}
		got, err := windows.LoadResourceData(module, resource)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("resource %d/%d differs from source: %v", kind, id, err)
		}
	}
	check(14, 1, group)
	for i, image := range images {
		check(3, uint16(i+1), image)
	}
	if output, err := exec.Command(exe, "-test.run=^$").CombinedOutput(); err != nil {
		t.Fatalf("executable no longer runs after embedding icon: %v\n%s", err, output)
	}
}

func TestNativePNGCreatesWindowsIcon(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "internal", "desktop", "app-icon.png"))
	if err != nil {
		t.Fatal(err)
	}
	user := windows.NewLazySystemDLL("user32.dll")
	create := user.NewProc("CreateIconFromResourceEx")
	destroy := user.NewProc("DestroyIcon")
	for _, size := range []uintptr{16, 32, 48, 512} {
		icon, _, err := create.Call(uintptr(unsafe.Pointer(&data[0])), uintptr(len(data)), 1, 0x00030000, size, size, 0)
		runtime.KeepAlive(data)
		if icon == 0 {
			t.Fatalf("cannot create %dpx native icon: %v", size, err)
		}
		if ok, _, err := destroy.Call(icon); ok == 0 {
			t.Fatalf("cannot release icon: %v", err)
		}
	}
}
