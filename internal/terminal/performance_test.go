package terminal

import (
	"fmt"
	"image"
	"runtime"
	"strings"
	"testing"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
)

var terminalBenchmarkSink any

func populatedBenchmarkScreen(width, height int) Screen {
	screen := NewScreen(width, height)
	for y := range height {
		text := ansi.Truncate(strings.Repeat("a界e\u0301 ", width), width-8, "")
		_, _ = fmt.Fprintf(screen, "\x1b[%d;1H\x1b[32m%03d\x1b[0m %s\x1b[44m  \x1b[0mZ", y+1, y, text)
	}
	return screen
}

func populatedBenchmarkBuffer(width, height int) *uv.RenderBuffer {
	screen := populatedBenchmarkScreen(width, height).(*emulatorScreen)
	buffer := uv.NewRenderBuffer(width, height)
	for y := range height {
		copyBenchmarkRow(buffer.Line(y), screen, y)
		buffer.TouchLine(0, y, width)
	}
	return buffer
}

func copyBenchmarkRow(row uv.Line, screen *emulatorScreen, y int) {
	for x := range row {
		row[x] = *screen.emulator.CellAt(x, y)
	}
}

func restoreBenchmarkBuffer(destination, source *uv.RenderBuffer) {
	for y := range source.Lines {
		copy(destination.Line(y), source.Line(y))
	}
}

func BenchmarkPopulatedDeleteRow(b *testing.B) {
	for _, size := range [][2]int{{80, 24}, {120, 40}, {200, 50}} {
		b.Run(fmt.Sprintf("%dx%d", size[0], size[1]), func(b *testing.B) { benchmarkDeleteGeometry(b, size[0], size[1]) })
	}
}

func benchmarkDeleteGeometry(b *testing.B, width, height int) {
	for _, partial := range []bool{false, true} {
		b.Run(fmt.Sprintf("partial=%v", partial), func(b *testing.B) { benchmarkPopulatedDeleteRow(b, width, height, partial) })
	}
}

func benchmarkPopulatedDeleteRow(b *testing.B, width, height int, partial bool) {
	const batch = 16
	source := populatedBenchmarkBuffer(width, height)
	buffers := make([]*uv.RenderBuffer, batch)
	for index := range buffers {
		buffers[index] = populatedBenchmarkBuffer(width, height)
	}
	area := image.Rect(0, 1, width, height-1)
	if partial {
		area.Min.X, area.Max.X = 1, width-1
	}
	b.ReportAllocs()
	b.ResetTimer()
	for done := 0; done < b.N; {
		b.StopTimer()
		for _, buffer := range buffers {
			restoreBenchmarkBuffer(buffer, source)
		}
		count := min(batch, b.N-done)
		b.StartTimer()
		for _, buffer := range buffers[:count] {
			buffer.DeleteLineArea(1, 1, nil, area)
		}
		done += count
	}
	b.StopTimer()
	b.ReportMetric(1, "deleted-rows/op")
	b.ReportMetric(float64(area.Dx()*(area.Dy()-1)), "survivor-cells/op")
	terminalBenchmarkSink = buffers[0]
}

func BenchmarkSaturatedScrollbackPush(b *testing.B) {
	for _, used := range []int{72, 120} {
		b.Run(fmt.Sprintf("used-%d-of-120", used), func(b *testing.B) { benchmarkSaturatedPush(b, used) })
	}
}

func benchmarkSaturatedPush(b *testing.B, used int) {
	buffer := populatedBenchmarkBuffer(used, 40)
	line := uv.NewLine(120)
	copy(line, buffer.Line(17))
	line[used-1] = uv.Cell{Content: "Z", Width: 1}
	history := vt.NewScrollback(256)
	for range 300 {
		history.Push(line)
	}
	inputBytes := len(line[:used].String())
	b.ReportAllocs()
	for b.Loop() {
		history.Push(line)
	}
	b.ReportMetric(1, "pushed-rows/op")
	b.ReportMetric(float64(inputBytes), "input-B/op")
	if history.Len() != 256 {
		b.Fatal(history.Len())
	}
	terminalBenchmarkSink = history
}

func BenchmarkTerminalMixedScroll(b *testing.B) {
	screen := populatedBenchmarkScreen(120, 40)
	data := []byte(corpusHistory(512))
	_, _ = screen.Write(data)
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for b.Loop() {
		_, _ = screen.Write(data)
	}
	b.ReportMetric(512, "input-rows/op")
	terminalBenchmarkSink = screen
}

func terminalConstruction(kind string) any {
	switch kind {
	case "ansi-default":
		return ansi.NewParser()
	case "ansi-4MiB":
		parser := ansi.NewParser()
		parser.SetDataSize(4 << 20)
		return parser
	case "vt-120x40":
		emulator := vt.NewEmulator(120, 40)
		_ = emulator.Close()
		return emulator
	default:
		return NewScreen(120, 40)
	}
}

func BenchmarkTerminalConstructor(b *testing.B) {
	for _, kind := range []string{"ansi-default", "ansi-4MiB", "vt-120x40", "mesh-120x40"} {
		b.Run(kind, func(b *testing.B) { benchmarkTerminalConstructor(b, kind) })
	}
}

func benchmarkTerminalConstructor(b *testing.B, kind string) {
	b.ReportAllocs()
	for b.Loop() {
		terminalBenchmarkSink = terminalConstruction(kind)
	}
}

func BenchmarkTerminalLiveHeapBatch(b *testing.B) {
	for _, kind := range []string{"ansi-default", "ansi-4MiB", "vt-120x40", "mesh-120x40"} {
		b.Run(kind, func(b *testing.B) { benchmarkTerminalLiveHeap(b, kind) })
	}
}

func benchmarkTerminalLiveHeap(b *testing.B, kind string) {
	const batch = 20
	terminalBenchmarkSink = nil
	objects := make([]any, batch)
	var retained float64
	for b.Loop() {
		clear(objects)
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		for index := range objects {
			objects[index] = terminalConstruction(kind)
		}
		runtime.GC()
		runtime.ReadMemStats(&after)
		retained += float64(after.HeapAlloc) - float64(before.HeapAlloc)
		runtime.KeepAlive(objects)
	}
	b.ReportMetric(retained/float64(b.N*batch), "live-B/object")
	b.ReportMetric(batch, "objects/op")
}

func TestPopulatedBenchmarkFixtures(t *testing.T) {
	buffer := populatedBenchmarkBuffer(80, 24)
	before := buffer.Line(2).String()
	buffer.DeleteLineArea(1, 1, nil, image.Rect(0, 1, 80, 23))
	if before != buffer.Line(1).String() || !strings.Contains(before, "界") {
		t.Fatal("row deletion benchmark did not move populated mixed-width data")
	}
}

func BenchmarkTerminalControlPayload(b *testing.B) {
	for _, size := range []int{1024, 64 << 10, (4 << 20) + 1} {
		b.Run(fmt.Sprint(size), func(b *testing.B) { benchmarkTerminalControlPayload(b, size) })
	}
}

func benchmarkTerminalControlPayload(b *testing.B, size int) {
	screen := NewScreen(120, 40)
	data := []byte("\x1b]2;" + strings.Repeat("x", size) + "\x07\x1b]8;;\x07")
	_, _ = screen.Write(data)
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for b.Loop() {
		_, _ = screen.Write(data)
	}
	terminalBenchmarkSink = screen
}
