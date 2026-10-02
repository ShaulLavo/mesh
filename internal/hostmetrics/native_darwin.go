//go:build darwin

package hostmetrics

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/ebitengine/purego"
)

// Handles name OS libraries only; sensor clients and registry objects are released per read.
type darwinNative struct {
	release          func(uintptr)
	typeID           func(uintptr) uintptr
	numberType       func() uintptr
	booleanType      func() uintptr
	stringType       func() uintptr
	dictionaryType   func() uintptr
	stringCreate     func(uintptr, string, uint32) uintptr
	stringCopy       func(uintptr, *byte, int64, uint32) bool
	numberCreate     func(uintptr, int64, *int64) uintptr
	numberValue      func(uintptr, int64, *float64) bool
	booleanValue     func(uintptr) bool
	dictionaryCreate func(uintptr, int64, uintptr, uintptr) uintptr
	dictionarySet    func(uintptr, uintptr, uintptr)
	dictionaryGet    func(uintptr, uintptr) uintptr
	arrayCount       func(uintptr) int64
	arrayValue       func(uintptr, int64) uintptr
	matching         func(string) uintptr
	matchingServices func(uint32, uintptr, *uint32) int32
	iteratorNext     func(uint32) uint32
	objectRelease    func(uint32) int32
	property         func(uint32, uintptr, uintptr, uint32) uintptr
	hidCreate        func(uintptr) uintptr
	hidMatching      func(uintptr, uintptr) int32
	hidServices      func(uintptr) uintptr
	hidProperty      func(uintptr, uintptr) uintptr
	hidEvent         func(uintptr, int64, int32, int64) uintptr
	hidValue         func(uintptr, int64) float64
	hidError         error
}

var nativeOnce sync.Once
var native *darwinNative
var nativeError error

func nativeAPI() (*darwinNative, error) {
	nativeOnce.Do(func() { native, nativeError = loadNative() })
	return native, nativeError
}
func bindNative(library uintptr, name string, target any) error {
	symbol, err := purego.Dlsym(library, name)
	if err != nil {
		return fmt.Errorf("native symbol %s: %w", name, err)
	}
	purego.RegisterFunc(target, symbol)
	return nil
}
func loadNative() (*darwinNative, error) {
	cf, err := purego.Dlopen("/System/Library/Frameworks/CoreFoundation.framework/CoreFoundation", purego.RTLD_LAZY|purego.RTLD_LOCAL)
	if err != nil {
		return nil, fmt.Errorf("CoreFoundation: %w", err)
	}
	io, err := purego.Dlopen("/System/Library/Frameworks/IOKit.framework/IOKit", purego.RTLD_LAZY|purego.RTLD_LOCAL)
	if err != nil {
		_ = purego.Dlclose(cf)
		return nil, fmt.Errorf("IOKit: %w", err)
	}
	n := &darwinNative{}
	for _, binding := range []struct {
		name   string
		target any
	}{
		{"CFRelease", &n.release}, {"CFGetTypeID", &n.typeID}, {"CFNumberGetTypeID", &n.numberType}, {"CFBooleanGetTypeID", &n.booleanType}, {"CFStringGetTypeID", &n.stringType}, {"CFDictionaryGetTypeID", &n.dictionaryType},
		{"CFStringCreateWithCString", &n.stringCreate}, {"CFStringGetCString", &n.stringCopy}, {"CFNumberCreate", &n.numberCreate}, {"CFNumberGetValue", &n.numberValue}, {"CFBooleanGetValue", &n.booleanValue},
		{"CFDictionaryCreateMutable", &n.dictionaryCreate}, {"CFDictionarySetValue", &n.dictionarySet}, {"CFDictionaryGetValue", &n.dictionaryGet}, {"CFArrayGetCount", &n.arrayCount}, {"CFArrayGetValueAtIndex", &n.arrayValue},
	} {
		if err := bindNative(cf, binding.name, binding.target); err != nil {
			_ = purego.Dlclose(io)
			_ = purego.Dlclose(cf)
			return nil, err
		}
	}
	for _, binding := range []struct {
		name   string
		target any
	}{
		{"IOServiceMatching", &n.matching}, {"IOServiceGetMatchingServices", &n.matchingServices}, {"IOIteratorNext", &n.iteratorNext}, {"IOObjectRelease", &n.objectRelease}, {"IORegistryEntryCreateCFProperty", &n.property},
	} {
		if err := bindNative(io, binding.name, binding.target); err != nil {
			_ = purego.Dlclose(io)
			_ = purego.Dlclose(cf)
			return nil, err
		}
	}
	for _, binding := range []struct {
		name   string
		target any
	}{
		{"IOHIDEventSystemClientCreate", &n.hidCreate}, {"IOHIDEventSystemClientSetMatching", &n.hidMatching}, {"IOHIDEventSystemClientCopyServices", &n.hidServices},
		{"IOHIDServiceClientCopyProperty", &n.hidProperty}, {"IOHIDServiceClientCopyEvent", &n.hidEvent}, {"IOHIDEventGetFloatValue", &n.hidValue},
	} {
		if err := bindNative(io, binding.name, binding.target); err != nil {
			n.hidError = err
			break
		}
	}
	return n, nil
}

const cfUTF8 = 0x08000100

func (n *darwinNative) text(value uintptr) string {
	if value == 0 || n.typeID(value) != n.stringType() {
		return ""
	}
	var data [512]byte
	if !n.stringCopy(value, &data[0], int64(len(data)), cfUTF8) {
		return ""
	}
	return strings.TrimRight(string(data[:]), "\x00")
}
func (n *darwinNative) number(value uintptr) (float64, bool) {
	if value == 0 || n.typeID(value) != n.numberType() {
		return 0, false
	}
	var result float64
	ok := n.numberValue(value, 6, &result)
	return result, ok && finite(result)
}
func (n *darwinNative) get(dictionary uintptr, key string) uintptr {
	if dictionary == 0 || n.typeID(dictionary) != n.dictionaryType() {
		return 0
	}
	name := n.stringCreate(0, key, cfUTF8)
	if name == 0 {
		return 0
	}
	defer n.release(name)
	return n.dictionaryGet(dictionary, name)
}
func (n *darwinNative) field(service uint32, key string) uintptr {
	name := n.stringCreate(0, key, cfUTF8)
	if name == 0 {
		return 0
	}
	defer n.release(name)
	return n.property(service, name, 0, 0)
}
func (n *darwinNative) fieldNumber(service uint32, key string) (float64, bool) {
	value := n.field(service, key)
	if value == 0 {
		return 0, false
	}
	defer n.release(value)
	return n.number(value)
}
func (n *darwinNative) fieldBool(service uint32, key string) bool {
	value := n.field(service, key)
	if value == 0 {
		return false
	}
	defer n.release(value)
	return n.typeID(value) == n.booleanType() && n.booleanValue(value)
}
func (n *darwinNative) services(class string, read func(uint32) error) error {
	match := n.matching(class)
	if match == 0 {
		return ErrUnsupported
	}
	var iterator uint32
	// IOServiceGetMatchingServices consumes the matching dictionary.
	if status := n.matchingServices(0, match, &iterator); status != 0 {
		return fmt.Errorf("IOKit matching %s: status %d", class, status)
	}
	defer n.objectRelease(iterator)
	found := false
	for range 64 {
		service := n.iteratorNext(iterator)
		if service == 0 {
			if !found {
				return ErrUnsupported
			}
			return nil
		}
		found = true
		err := read(service)
		n.objectRelease(service)
		if err != nil {
			return err
		}
	}
	return errors.New("IOKit service count exceeds bound")
}
