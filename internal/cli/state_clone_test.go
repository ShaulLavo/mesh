package cli

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
)

// Every field is filled by reflection so a field added to a session, service
// or view type later is covered without editing this test.
func TestStateViewCloneDeepOwnership(t *testing.T) {
	var view StateView
	fillAll(reflect.ValueOf(&view).Elem(), 1)
	cloned := view.Clone()
	if !reflect.DeepEqual(view, cloned) {
		t.Fatalf("view clone differs:\nsource %#v\nclone  %#v", view, cloned)
	}
	assertNoSharedStorage(t, "StateView", reflect.ValueOf(view), reflect.ValueOf(cloned))
}

func TestCatalogClonesOwnRows(t *testing.T) {
	for _, size := range []int{1, 0} {
		var sessions []protocol.SessionInfo
		var services []protocol.ServiceInfo
		fillAll(reflect.ValueOf(&sessions).Elem(), 1)
		fillAll(reflect.ValueOf(&services).Elem(), 1)
		fillAll(reflect.ValueOf(&sessions[0]).Elem(), size)
		fillAll(reflect.ValueOf(&services[0]).Elem(), size)
		assertOwnedClone(t, sessions, cloneSessionInfo(sessions))
		assertOwnedClone(t, services, cloneWireServices(services))
	}
	if cloneSessionInfo(nil) != nil || cloneWireServices(nil) != nil {
		t.Fatal("clone of nil rows is non-nil")
	}
	assertOwnedClone(t, []protocol.SessionInfo{{}}, cloneSessionInfo([]protocol.SessionInfo{{}}))
	assertOwnedClone(t, []protocol.ServiceInfo{{}}, cloneWireServices([]protocol.ServiceInfo{{}}))
}

func assertOwnedClone[T any](t *testing.T, source, cloned T) {
	t.Helper()
	if !reflect.DeepEqual(source, cloned) {
		t.Fatalf("clone differs from source:\nsource %#v\nclone  %#v", source, cloned)
	}
	want, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(cloned)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("clone encodes as %s, source as %s", got, want)
	}
	assertNoSharedStorage(t, reflect.TypeFor[T]().String(), reflect.ValueOf(source), reflect.ValueOf(cloned))
}

var timeType = reflect.TypeFor[time.Time]()

// fillAll sets every exported field below v. Slices and maps get size
// entries, so size 0 builds non-nil empty collections.
func fillAll(v reflect.Value, size int) {
	switch v.Kind() {
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		fillAll(v.Elem(), size)
	case reflect.Struct:
		fillStruct(v, size)
	case reflect.Slice:
		v.Set(reflect.MakeSlice(v.Type(), size, size))
		for i := range size {
			fillAll(v.Index(i), size)
		}
	case reflect.Map:
		v.Set(reflect.MakeMap(v.Type()))
		if size > 0 {
			key, value := reflect.New(v.Type().Key()).Elem(), reflect.New(v.Type().Elem()).Elem()
			fillAll(key, size)
			fillAll(value, size)
			v.SetMapIndex(key, value)
		}
	case reflect.String:
		v.SetString("value")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(7)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(7)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(7)
	case reflect.Invalid, reflect.Uintptr, reflect.Complex64, reflect.Complex128, reflect.Array, reflect.Chan, reflect.Func, reflect.Interface, reflect.UnsafePointer:
		// A new field of these kinds needs its own fill and clone rule.
		panic("fillAll: unsupported kind " + v.Kind().String())
	}
}

func fillStruct(v reflect.Value, size int) {
	if v.Type() == timeType {
		v.Set(reflect.ValueOf(time.Unix(1700000000, 0).UTC()))
		return
	}
	for i := range v.NumField() {
		if v.Field(i).CanSet() {
			fillAll(v.Field(i), size)
		}
	}
}

// assertNoSharedStorage fails when a pointer, slice or map in cloned refers to
// the same storage as source.
func assertNoSharedStorage(t *testing.T, path string, source, cloned reflect.Value) {
	t.Helper()
	switch source.Kind() {
	case reflect.Pointer:
		if source.IsNil() {
			return
		}
		if source.Pointer() == cloned.Pointer() {
			t.Errorf("%s shares storage with its clone", path)
			return
		}
		assertNoSharedStorage(t, path, source.Elem(), cloned.Elem())
	case reflect.Slice:
		if source.Cap() > 0 && source.Pointer() == cloned.Pointer() {
			t.Errorf("%s shares storage with its clone", path)
			return
		}
		for i := range min(source.Len(), cloned.Len()) {
			assertNoSharedStorage(t, path+"[]", source.Index(i), cloned.Index(i))
		}
	case reflect.Map:
		if !source.IsNil() && source.Pointer() == cloned.Pointer() {
			t.Errorf("%s shares storage with its clone", path)
			return
		}
		for _, key := range source.MapKeys() {
			assertNoSharedStorage(t, path+"[key]", source.MapIndex(key), cloned.MapIndex(key))
		}
	case reflect.Struct:
		if source.Type() == timeType {
			return
		}
		for i := range source.NumField() {
			assertNoSharedStorage(t, path+"."+source.Type().Field(i).Name, source.Field(i), cloned.Field(i))
		}
	case reflect.Invalid, reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr, reflect.Float32, reflect.Float64, reflect.Complex64, reflect.Complex128, reflect.Array, reflect.Chan, reflect.Func, reflect.Interface, reflect.String, reflect.UnsafePointer:
		// Scalars hold no shared storage, and fillAll rejects the other kinds.
	}
}
