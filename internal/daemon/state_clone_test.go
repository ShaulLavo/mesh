package daemon

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/hostmetrics"
	"github.com/shaul/mesh/internal/protocol"
)

// Every field is filled by reflection so a field added to a payload type later
// is covered without editing this test.
func TestStateCloneDeepOwnership(t *testing.T) {
	var source protocol.StateEvent
	fillAll(reflect.ValueOf(&source).Elem(), 1)
	assertOwnedClone(t, source, cloneStateEvent(source))
	var empty protocol.StatePayload
	fillAll(reflect.ValueOf(&empty).Elem(), 0)
	// AgeSnapshot owns metric copies and has always dropped empty metric slices.
	empty.Metrics = nil
	assertOwnedClone(t, empty, cloneStatePayload(empty))
	assertOwnedClone(t, protocol.StatePayload{}, cloneStatePayload(protocol.StatePayload{}))
}

func TestStateSnapshotOwnsBrokerRows(t *testing.T) {
	broker := newStateBroker(1, time.Now)
	var host protocol.HostInfo
	var session protocol.SessionInfo
	var service protocol.ServiceInfo
	fillAll(reflect.ValueOf(&host).Elem(), 1)
	fillAll(reflect.ValueOf(&session).Elem(), 1)
	fillAll(reflect.ValueOf(&service).Elem(), 1)
	broker.host = &host
	broker.sessions[session.ID] = session
	broker.services[service.Name] = service
	broker.memory[session.ID] = protocol.SessionMemory{Bytes: 1}
	sub := &stateSubscriber{topics: map[string]bool{protocol.TopicHost: true, protocol.TopicSessions: true, protocol.TopicServices: true}}
	snapshot := broker.snapshotLocked(sub)
	assertOwnedClone(t, host, *snapshot.Host)
	assertOwnedClone(t, session, snapshot.Sessions[0])
	assertOwnedClone(t, service, snapshot.Services[0])
	assertNoSharedStorage(t, "memory", reflect.ValueOf(broker.memory), reflect.ValueOf(snapshot.Memory))
	empty := newStateBroker(1, time.Now).snapshotLocked(&stateSubscriber{topics: sub.topics})
	if empty.Sessions != nil || empty.Services != nil {
		t.Fatalf("snapshot of an empty broker has rows: %#v", empty)
	}
}

func TestStateTakeAgesOwnedMetrics(t *testing.T) {
	now := time.Unix(1700000000, 0)
	broker := newStateBroker(1, func() time.Time { return now })
	sub, _, err := broker.subscribe(protocol.StateWatch{Topics: []string{protocol.TopicMetrics}})
	if err != nil {
		t.Fatal(err)
	}
	var metrics hostmetrics.Snapshot
	fillAll(reflect.ValueOf(&metrics).Elem(), 1)
	broker.metricsChanged(metrics)
	now = now.Add(time.Second)
	controls := broker.take(sub)
	if len(controls) != 1 || controls[0].StateEvent == nil {
		t.Fatalf("take = %#v, want one metrics event", controls)
	}
	sent := controls[0].StateEvent.Payload.Metrics
	if sent.CPU.AgeMillis != metrics.CPU.AgeMillis+1000 || broker.metrics.CPU.AgeMillis != metrics.CPU.AgeMillis {
		t.Fatalf("sent CPU age %d, broker CPU age %d, want %d and %d", sent.CPU.AgeMillis, broker.metrics.CPU.AgeMillis, metrics.CPU.AgeMillis+1000, metrics.CPU.AgeMillis)
	}
	assertNoSharedStorage(t, "metrics", reflect.ValueOf(broker.metrics), reflect.ValueOf(sent))
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
