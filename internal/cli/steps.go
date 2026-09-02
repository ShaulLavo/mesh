package cli

import (
	"fmt"
	"io"
	"sync"
	"time"

	"charm.land/bubbles/v2/spinner"
	"github.com/charmbracelet/x/term"
)

// StepPrinter draws a live line for the step in flight and leaves a plain
// finished line behind when the next one starts. Steps here are remote work
// over SSH, so several of them sit for seconds with nothing to show.
type StepPrinter struct {
	output io.Writer
	label  string
	live   bool

	mu      sync.Mutex
	current string
	frame   int
	stop    chan struct{}
	stopped sync.WaitGroup
}

// NewStepPrinter animates only for a terminal. Redirected output keeps the
// plain one-line-per-step form, which is what a log wants.
func NewStepPrinter(output io.Writer, label string) *StepPrinter {
	file, ok := output.(term.File)
	return &StepPrinter{output: output, label: label, live: ok && term.IsTerminal(file.Fd())}
}

// Step finishes the previous line and begins one for step.
func (p *StepPrinter) Step(step, detail string) {
	p.finish()
	line := fmt.Sprintf("%s %-9s %s", Tag(p.output, p.label), SafeTerminalText(step), SafeTerminalText(detail))
	if !p.live {
		_, _ = fmt.Fprintln(p.output, line)
		return
	}
	p.mu.Lock()
	p.current = line
	p.frame = 0
	p.stop = make(chan struct{})
	stop := p.stop
	p.mu.Unlock()

	p.stopped.Add(1)
	go func() {
		defer p.stopped.Done()
		ticker := time.NewTicker(spinner.MiniDot.FPS)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				p.draw()
			}
		}
	}()
	p.draw()
}

// Pause finishes the current line without starting another. A prompt takes the
// terminal over, and a spinner still redrawing under it corrupts both.
func (p *StepPrinter) Pause() { p.finish() }

// Done finishes the last line.
func (p *StepPrinter) Done() { p.finish() }

func (p *StepPrinter) draw() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.current == "" {
		return
	}
	frames := spinner.MiniDot.Frames
	_, _ = fmt.Fprintf(p.output, "\r%s %s\x1b[K", p.current, frames[p.frame%len(frames)])
	p.frame++
}

func (p *StepPrinter) finish() {
	p.mu.Lock()
	stop, line := p.stop, p.current
	p.stop, p.current = nil, ""
	p.mu.Unlock()
	if stop != nil {
		close(stop)
		p.stopped.Wait()
	}
	if line == "" {
		return
	}
	_, _ = fmt.Fprintf(p.output, "\r%s\x1b[K\n", line)
}
