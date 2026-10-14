// Package ui is rcmanager's terminal interaction: spinners, live task lists and
// prompts.
//
// Everything here is CLI-only. The daemon must never prompt: it has no
// terminal, and a supervisor that blocks on a question nobody can see is the
// exact failure rcmanager exists to prevent. So the CLI asks, decides, and hands
// the daemon a settled answer.
package ui

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dumanhaydar/rcmanager/internal/ansi"
)

var frames = []string{"⣋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

const (
	interval   = 80 * time.Millisecond
	clearLine  = "\r\x1b[2K"
	hideCursor = "\x1b[?25l"
	showCursor = "\x1b[?25h"
)

// OK, Info and Warn are rcmanager's three report lines.
func OK(msg string)   { fmt.Printf("%s %s\n", ansi.Green("✔"), msg) }
func Info(msg string) { fmt.Printf("%s %s\n", ansi.Grey("·"), msg) }
func Warn(msg string) { fmt.Printf("%s %s\n", ansi.Yellow("!"), msg) }

// Spinner is a one-line spinner for work that takes long enough to look like
// a hang. It is silent without a TTY: piped output and CI logs get one plain
// line per step instead of a few hundred cursor-return frames.
type Spinner struct {
	mu    sync.Mutex
	label string
	tty   bool
	stop  chan struct{}
	done  chan struct{}
}

// NewSpinner starts a spinner with the given label.
func NewSpinner(label string) *Spinner {
	s := &Spinner{label: label, tty: ansi.IsTerminal(os.Stdout)}
	if !s.tty {
		fmt.Printf("  %s...\n", label)
		return s
	}
	s.stop = make(chan struct{})
	s.done = make(chan struct{})
	go s.run()
	return s
}

func (s *Spinner) run() {
	defer close(s.done)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	frame := 0
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			frame = (frame + 1) % len(frames)
			s.mu.Lock()
			fmt.Printf("%s  %s %s", clearLine, ansi.Cyan(frames[frame]), s.label)
			s.mu.Unlock()
		}
	}
}

// Update changes the text without restarting the animation.
func (s *Spinner) Update(label string) {
	s.mu.Lock()
	s.label = label
	s.mu.Unlock()
	if !s.tty {
		fmt.Printf("  %s...\n", label)
	}
}

func (s *Spinner) finish(mark, text string) {
	if s.stop != nil {
		close(s.stop)
		<-s.done
		s.stop = nil
		fmt.Print(clearLine)
	}
	if text == "" {
		s.mu.Lock()
		text = s.label
		s.mu.Unlock()
	}
	fmt.Printf("  %s %s\n", mark, text)
}

// Succeed, Fail and Note replace the spinner with a final line.
func (s *Spinner) Succeed(text string) { s.finish(ansi.Green("✔"), text) }
func (s *Spinner) Fail(text string)    { s.finish(ansi.Red("✖"), text) }
func (s *Spinner) Note(text string)    { s.finish(ansi.Grey("·"), text) }

// WithSpinner runs fn under a spinner, which is always stopped: one left
// running over a returned error would sit on top of the message.
func WithSpinner[T any](label string, fn func() (T, error)) (T, error) {
	s := NewSpinner(label)
	result, err := fn()
	if err != nil {
		s.Fail(label + " — " + err.Error())
		return result, err
	}
	s.Succeed("")
	return result, nil
}

// TaskState is where one task in a TaskList stands.
type TaskState string

const (
	Pending TaskState = "pending"
	Active  TaskState = "active"
	Done    TaskState = "done"
	Failed  TaskState = "failed"
	Skipped TaskState = "skipped"
)

type task struct {
	key    string
	label  string
	state  TaskState
	detail string
}

// TaskList is a live list of tasks, repainted in place.
//
// `rcmanager restart all` takes as long as restarting every session takes, and a
// request/response pair can only report that once it is over. With the daemon
// streaming progress, this is the other half: one line per session, updated
// where it stands, so a slow operation looks like work rather than a hang.
//
// Without a TTY it degrades to one line per finished task — a piped log should
// be readable afterwards, not a transcript of an animation.
type TaskList struct {
	mu      sync.Mutex
	tasks   []*task
	tty     bool
	width   int
	frame   int
	painted int
	stop    chan struct{}
	done    chan struct{}
}

// NewTaskList starts a live list with one line per name.
func NewTaskList(names []string) *TaskList {
	t := &TaskList{tty: ansi.IsTerminal(os.Stdout)}
	for _, n := range names {
		t.tasks = append(t.tasks, &task{key: n, label: n, state: Pending})
		if w := ansi.VisibleLength(n); w > t.width {
			t.width = w
		}
	}
	if !t.tty {
		return t
	}
	fmt.Print(hideCursor)
	t.paint()
	t.stop = make(chan struct{})
	t.done = make(chan struct{})
	go func() {
		defer close(t.done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-t.stop:
				return
			case <-ticker.C:
				t.mu.Lock()
				t.frame = (t.frame + 1) % len(frames)
				t.mu.Unlock()
				t.paint()
			}
		}
	}()
	return t
}

// Set updates one task. An unknown key is ignored rather than fatal.
func (t *TaskList) Set(key string, state TaskState, detail string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	var found *task
	for _, task := range t.tasks {
		if task.key == key {
			found = task
			break
		}
	}
	if found == nil {
		t.mu.Unlock()
		return
	}
	found.state = state
	found.detail = detail
	line := t.lineOf(found)
	t.mu.Unlock()

	if t.tty {
		t.paint()
		return
	}
	// Without a tty, report each task once it settles.
	if state != Pending && state != Active {
		fmt.Println(line)
	}
}

func (t *TaskList) lineOf(task *task) string {
	var mark string
	switch task.state {
	case Active:
		mark = ansi.Cyan(frames[t.frame])
	case Done:
		mark = ansi.Green("✔")
	case Failed:
		mark = ansi.Red("✖")
	case Skipped:
		mark = ansi.Grey("-")
	default:
		mark = ansi.Grey("·")
	}
	label := task.label
	if task.state == Pending {
		label = ansi.Grey(label)
	}
	pad := strings.Repeat(" ", max(0, t.width-ansi.VisibleLength(task.label)))
	detail := ""
	if task.detail != "" {
		detail = "  " + ansi.Grey(task.detail)
	}
	return "  " + mark + " " + label + pad + detail
}

func (t *TaskList) paint() {
	if !t.tty {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	var b strings.Builder
	if t.painted > 0 {
		b.WriteString("\x1b[" + strconv.Itoa(t.painted) + "A\r")
	}
	for i, task := range t.tasks {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString("\x1b[2K" + t.lineOf(task))
	}
	b.WriteByte('\n')
	fmt.Print(b.String())
	t.painted = len(t.tasks)
}

// Stop ends the animation and leaves the finished list on screen.
func (t *TaskList) Stop() {
	if t == nil || t.stop == nil {
		return
	}
	close(t.stop)
	<-t.done
	t.stop = nil
	t.paint()
	fmt.Print(showCursor)
}

// Interactive reports whether there is a human to ask.
func Interactive() bool {
	return ansi.IsTerminal(os.Stdin) && ansi.IsTerminal(os.Stdout)
}

var stdin = bufio.NewReader(os.Stdin)

// Ask is a free-text question. It returns fallback on an empty answer or EOF.
func Ask(question, fallback string) string {
	suffix := ""
	if fallback != "" {
		suffix = ansi.Grey(" (" + fallback + ")")
	}
	fmt.Printf("%s %s%s ", ansi.Cyan("?"), question, suffix)

	line, err := stdin.ReadString('\n')
	if err != nil && line == "" {
		fmt.Println()
		return fallback
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return fallback
	}
	return line
}

// Confirm is a yes/no question. def is what Enter means, and is shown
// capitalised so the default is visible rather than something to remember.
func Confirm(question string, def bool) bool {
	hint := "y/N"
	if def {
		hint = "Y/n"
	}
	answer := strings.ToLower(Ask(question+" "+ansi.Grey("["+hint+"]"), ""))
	if answer == "" {
		return def
	}
	return answer == "y" || answer == "yes"
}

// Choice is one option in a Select.
type Choice struct {
	Value string
	Label string
	Hint  string
}

// Select picks one of a list.
//
// Arrow keys would need raw mode and a redraw loop; a numbered list works over
// ssh, in tmux, and in every terminal that ever mangled an escape sequence.
func Select(question string, choices []Choice, def int) string {
	fmt.Printf("%s %s\n", ansi.Cyan("?"), question)
	for i, c := range choices {
		marker := " "
		if i == def {
			marker = ansi.Cyan("❯")
		}
		hint := ""
		if c.Hint != "" {
			hint = ansi.Grey("  " + c.Hint)
		}
		fmt.Printf("  %s %s. %s%s\n", marker, ansi.Bold(strconv.Itoa(i+1)), c.Label, hint)
	}
	for {
		raw := Ask(fmt.Sprintf("  choose 1-%d", len(choices)), strconv.Itoa(def+1))
		n, err := strconv.Atoi(raw)
		if err == nil && n >= 1 && n <= len(choices) {
			return choices[n-1].Value
		}
		fmt.Printf("  %s enter a number between 1 and %d\n", ansi.Red("✖"), len(choices))
	}
}

// MultiSelect picks any number of a list, by number. "all" selects
// everything, blank none.
func MultiSelect(question string, choices []Choice) []string {
	fmt.Printf("%s %s %s\n", ansi.Cyan("?"), question,
		ansi.Grey(`(comma-separated numbers, "all", or blank for none)`))
	for i, c := range choices {
		hint := ""
		if c.Hint != "" {
			hint = ansi.Grey("  " + c.Hint)
		}
		fmt.Printf("    %s. %s%s\n", ansi.Bold(strconv.Itoa(i+1)), c.Label, hint)
	}
	for {
		raw := strings.ToLower(Ask("  choose", ""))
		if raw == "" {
			return nil
		}
		if raw == "all" {
			out := make([]string, 0, len(choices))
			for _, c := range choices {
				out = append(out, c.Value)
			}
			return out
		}
		var picked []string
		valid := true
		seen := map[int]bool{}
		for _, part := range strings.Split(raw, ",") {
			n, err := strconv.Atoi(strings.TrimSpace(part))
			if err != nil || n < 1 || n > len(choices) || seen[n] {
				if err != nil || n < 1 || n > len(choices) {
					valid = false
					break
				}
				continue
			}
			seen[n] = true
			picked = append(picked, choices[n-1].Value)
		}
		if valid {
			return picked
		}
		fmt.Printf("  %s use numbers between 1 and %d, or \"all\"\n", ansi.Red("✖"), len(choices))
	}
}

// Step titles a section, so a multi-step flow reads as steps rather than a
// wall.
func Step(n, total int, title string) {
	fmt.Printf("\n%s %s\n", ansi.Grey(fmt.Sprintf("[%d/%d]", n, total)), ansi.Bold(title))
}
