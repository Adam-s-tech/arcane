package output

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// Confirm uses a terminal selector, falling back to line input for scripts.
func Confirm(ctx context.Context, input io.Reader, out io.Writer, label string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	defer SuspendProgress()()
	if !IsTerminal(input) || !IsTerminal(out) {
		if _, err := fmt.Fprintf(out, "%s (y/N): ", strings.TrimSpace(label)); err != nil {
			return false, err
		}
		var answer string
		if _, err := fmt.Fscanln(input, &answer); err != nil && !errors.Is(err, io.EOF) {
			return false, fmt.Errorf("failed to read confirmation input: %w", err)
		}
		if err := ctx.Err(); err != nil {
			return false, err
		}
		return strings.EqualFold(answer, "y") || strings.EqualFold(answer, "yes"), nil
	}
	model, err := tea.NewProgram(confirmModel{label: label, out: out}, tea.WithContext(ctx), tea.WithInput(input), tea.WithOutput(out)).Run()
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if errors.Is(err, tea.ErrInterrupted) {
		return false, context.Canceled
	}
	if err != nil {
		return false, fmt.Errorf("confirmation prompt: %w", err)
	}
	result, ok := model.(confirmModel)
	if !ok {
		return false, errors.New("confirmation returned an unexpected result")
	}
	return result.confirmed, nil
}

type confirmModel struct {
	label                string
	out                  io.Writer
	yes, confirmed, done bool
}

func (m confirmModel) Init() tea.Cmd { return nil }

func (m confirmModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if m.done {
		return m, nil
	}
	if key, ok := msg.(tea.KeyPressMsg); ok {
		switch strings.ToLower(key.String()) {
		case "left", "right", "up", "down", "tab", "shift+tab":
			m.yes = !m.yes
		case "y":
			m.yes = true
		case "n":
			m.yes = false
		case "enter":
			m.done, m.confirmed = true, m.yes
			return m, tea.Quit
		case "esc", "q":
			m.done = true
			return m, tea.Quit
		case "ctrl+c":
			m.done = true
			return m, tea.Interrupt
		}
	}
	return m, nil
}

func (m confirmModel) View() tea.View {
	if m.done {
		return tea.NewView("")
	}
	no, yes := "  No  ", "  Yes  "
	if m.yes {
		yes = "[ Yes ]"
	} else {
		no = "[ No ]"
	}
	hint := renderForInternal(m.out, statusMutedStyle, "←/→ or tab: choose · enter: submit · esc: cancel")
	return tea.NewView("\n  " + renderForInternal(m.out, headerStyle, m.label) + "\n\n  " + no + "  " + yes + "\n\n  " + hint + "\n")
}
