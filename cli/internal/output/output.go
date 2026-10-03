// Package output provides formatted terminal output utilities for the CLI.
//
// This package offers consistent styling for success messages, errors, warnings,
// informational text, headers, key-value pairs, and tables. All output includes
// appropriate color coding for better readability in terminal environments.
//
// # Example Usage
//
//	output.Success("Operation completed")
//	output.Warning("Something looks off: %v", err)
//	output.KeyValue("Status", "Running")
//	output.Table([]string{"ID", "Name"}, rows)
package output

import (
	"fmt"
	"image/color"
	"regexp"
	"strings"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"
	"github.com/charmbracelet/x/term"
	"github.com/mattn/go-runewidth"
	kit "go.getarcane.app/kit/pkg"
)

var (
	arcanePurple, textPrimary, textMuted, statusOnline, statusOffline, statusWarn                  color.Color
	successStyle, warnStyle, infoStyle, headerStyle, keyStyle, valueStyle                          lipgloss.Style
	statusOnlineStyle, statusOfflineStyle, statusWarnStyle, statusMutedStyle, enabledStyle         lipgloss.Style
	tableHeader, tableCell, tableOddRow, tableEvenRow, tableBorder, tablePlainCell, tablePlainHead lipgloss.Style
)

func init() { setThemeInternal(true) }

func setThemeInternal(dark bool) {
	c := lipgloss.LightDark(dark)
	arcanePurple = c(lipgloss.Color("#6d28d9"), lipgloss.Color("#a78bfa"))
	textPrimary = c(lipgloss.Color("#1f2937"), lipgloss.Color("#e5e7eb"))
	textMuted = c(lipgloss.Color("#64748b"), lipgloss.Color("#cbd5e1"))
	statusOnline = c(lipgloss.Color("#15803d"), lipgloss.Color("#4ade80"))
	statusOffline = c(lipgloss.Color("#b91c1c"), lipgloss.Color("#f87171"))
	statusWarn = c(lipgloss.Color("#b45309"), lipgloss.Color("#fbbf24"))
	successStyle = lipgloss.NewStyle().Foreground(statusOnline)
	warnStyle = lipgloss.NewStyle().Foreground(statusWarn)
	infoStyle = lipgloss.NewStyle().Foreground(arcanePurple)
	headerStyle = infoStyle.Bold(true)
	keyStyle = lipgloss.NewStyle().Foreground(textMuted)
	valueStyle = lipgloss.NewStyle().Foreground(textPrimary)
	statusOnlineStyle, statusOfflineStyle, statusWarnStyle = successStyle, lipgloss.NewStyle().Foreground(statusOffline), warnStyle
	statusMutedStyle = keyStyle
	enabledStyle = infoStyle
	tableCell = lipgloss.NewStyle().Padding(0, 1)
	tableHeader = tableCell.Foreground(arcanePurple).Bold(true).Align(lipgloss.Left)
	tableOddRow, tableEvenRow = tableCell.Foreground(textPrimary), tableCell.Foreground(textPrimary)
	tableBorder = lipgloss.NewStyle().Foreground(arcanePurple)
	tablePlainCell, tablePlainHead = tableCell, tableCell
}

var ansiRegexp = regexp.MustCompile("\x1b\\[[0-9;]*[a-zA-Z]")

// ansiReset closes a styled run re-applied after truncation.
const ansiReset = "\x1b[0m"

var tableWhitespaceReplacer = strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ", "\t", " ")

var colorEnabled = true

func shouldColorInternal() bool {
	return colorEnabledInternal() && IsTerminal(Stdout())
}

func renderInternal(style lipgloss.Style, value string) string {
	return renderForInternal(Stdout(), style, value)
}

// Success prints a success message in green.
// The message is prefixed with a newline for visual separation.
// Format specifiers and arguments work like fmt.Printf.
//
//nolint:goprintffuncname // printf-style output helper; *f rename across call sites tracked separately
func Success(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	defer SuspendProgress()()
	_, _ = fmt.Fprintf(Stdout(), "\n  %s\n", renderInternal(successStyle, "✓ "+msg))
}

// Warning prints a warning message in yellow.
// The message is prefixed with a newline for visual separation.
// Format specifiers and arguments work like fmt.Printf.
//
//nolint:goprintffuncname // printf-style output helper; *f rename across call sites tracked separately
func Warning(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	defer SuspendProgress()()
	_, _ = fmt.Fprintf(Stderr(), "\n  %s\n", renderForInternal(Stderr(), warnStyle, msg))
}

// Info prints an info message in purple.
// The message is prefixed with a newline for visual separation.
// Format specifiers and arguments work like fmt.Printf.
//
//nolint:goprintffuncname // printf-style output helper; *f rename across call sites tracked separately
func Info(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	defer SuspendProgress()()
	_, _ = fmt.Fprintf(Stdout(), "\n  %s\n", renderInternal(infoStyle, msg))
}

// Header prints a purple section heading.
// Use this to introduce sections of output. The message is prefixed
// with a newline for visual separation.
//
//nolint:goprintffuncname // printf-style output helper; *f rename across call sites tracked separately
func Header(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	defer SuspendProgress()()
	_, _ = fmt.Fprintf(Stdout(), "\n  %s\n", renderInternal(headerStyle, msg))
}

// KeyValue prints a key-value pair with the muted aligned labels and neutral values.
// This is useful for displaying structured information like image details
// or configuration values.
func KeyValue(key string, value any) {
	keyText := key
	valueText := fmt.Sprint(value)
	defer SuspendProgress()()
	_, _ = fmt.Fprintf(Stdout(), "  %s %s\n", renderInternal(keyStyle, fmt.Sprintf("%-18s", keyText)), renderInternal(valueStyle, valueText))
}

// Showing prints a pagination summary in the form "Showing: shown/total label".
func Showing(shown int, total int64, label string) {
	defer SuspendProgress()()
	_, _ = fmt.Fprintf(Stdout(), "\n  Showing: %d/%d %s\n", shown, total, label)
}

func hasAnsiInternal(s string) bool {
	if s == "" {
		return false
	}
	return ansiRegexp.MatchString(s)
}

// TintStatus applies semantic status coloring to a value.
func TintStatus(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || hasAnsiInternal(trimmed) || !shouldColorInternal() {
		return value
	}
	lower := strings.ToLower(trimmed)

	switch {
	case lower == "ok" || lower == "succeeded" || lower == "success" || lower == "online" || lower == "running" || lower == "healthy" || lower == "active" || strings.HasPrefix(lower, "up"):
		return statusOnlineStyle.Render(trimmed)
	case lower == "fail" || lower == "error" || lower == "offline" || lower == "stopped" || lower == "exited" ||
		lower == "dead" || lower == "unhealthy" || lower == "failed" || strings.HasPrefix(lower, "down"):
		return statusOfflineStyle.Render(trimmed)
	case lower == "warn" || lower == "warning" || lower == "paused" || lower == "restarting" || lower == "starting" || lower == "created" || lower == "degraded":
		return statusWarnStyle.Render(trimmed)
	default:
		return statusMutedStyle.Render(trimmed)
	}
}

// TintEnabled applies tints for enabled/disabled values.
func TintEnabled(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || hasAnsiInternal(trimmed) || !shouldColorInternal() {
		return value
	}
	lower := strings.ToLower(trimmed)
	switch lower {
	case "true", "yes", "enabled", "on":
		return enabledStyle.Render(trimmed)
	case "false", "no", "disabled", "off":
		return statusMutedStyle.Render(trimmed)
	default:
		return value
	}
}

// TintYesNo applies tints for yes/no style values.
func TintYesNo(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || hasAnsiInternal(trimmed) || !shouldColorInternal() {
		return value
	}
	lower := strings.ToLower(trimmed)
	switch lower {
	case "true", "yes", "y", "in use":
		return statusOnlineStyle.Render(trimmed)
	case "false", "no", "n":
		return statusMutedStyle.Render(trimmed)
	default:
		return value
	}
}

// TintInsecure applies warning tints for insecure values.
func TintInsecure(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || hasAnsiInternal(trimmed) || !shouldColorInternal() {
		return value
	}
	lower := strings.ToLower(trimmed)
	switch lower {
	case "true", "yes", "y", "insecure":
		return statusWarnStyle.Render(trimmed)
	case "false", "no", "n":
		return statusMutedStyle.Render(trimmed)
	default:
		return value
	}
}

// Table prints a formatted table with headers and rows.
// Rendering uses Lip Gloss table styles with zebra-striped rows.
func Table(headers []string, rows [][]string) error {
	defer SuspendProgress()()
	if _, spacingErr := fmt.Fprintln(Stdout()); spacingErr != nil {
		return fmt.Errorf("failed to print table: %w", spacingErr)
	}

	n := len(headers)
	if n == 0 {
		return nil
	}

	rows = normalizeTableRowsInternal(rows, n)
	rows = tintTableRowsInternal(headers, rows)
	headers, rows = fitTableToTerminalInternal(headers, rows)

	t := table.New().Border(lipgloss.NormalBorder()).Headers(headers...)

	if shouldColorInternal() {
		t = t.BorderStyle(tableBorder).
			StyleFunc(func(row, col int) lipgloss.Style {
				switch {
				case row == table.HeaderRow:
					return tableHeader
				case row%2 == 0:
					return tableEvenRow
				default:
					return tableOddRow
				}
			})
	} else {
		t = t.StyleFunc(func(row, col int) lipgloss.Style {
			return kit.Ternary(row == table.HeaderRow, tablePlainHead, tablePlainCell)
		})
	}

	if len(rows) > 0 {
		t = t.Rows(rows...)
	}

	if _, tableErr := fmt.Fprintln(Stdout(), t.String()); tableErr != nil {
		return fmt.Errorf("failed to print table: %w", tableErr)
	}
	return nil
}

func fitTableToTerminalInternal(headers []string, rows [][]string) ([]string, [][]string) {
	if len(headers) == 0 {
		return headers, rows
	}

	columnCount := len(headers)
	displayHeaders := make([]string, columnCount)
	for i, header := range headers {
		displayHeaders[i] = sanitizeTableCellInternal(header)
	}

	displayRows := normalizeTableRowsInternal(rows, columnCount)
	for i, row := range displayRows {
		cleaned := make([]string, columnCount)
		for col := range columnCount {
			cleaned[col] = sanitizeTableCellInternal(row[col])
		}
		displayRows[i] = cleaned
	}

	fd, ok := Stdout().(interface{ Fd() uintptr })
	if !ok || !term.IsTerminal(fd.Fd()) {
		return displayHeaders, displayRows
	}

	terminalWidth, _, err := term.GetSize(fd.Fd())
	if err != nil || terminalWidth <= 0 {
		return displayHeaders, displayRows
	}

	columnWidths := make([]int, columnCount)
	for i, header := range displayHeaders {
		columnWidths[i] = max(1, visibleWidthInternal(header))
	}

	for _, row := range displayRows {
		for col := range columnCount {
			columnWidths[col] = max(columnWidths[col], visibleWidthInternal(row[col]))
		}
	}

	availableContentWidth := terminalWidth - tableNonContentWidthInternal(columnCount)
	if availableContentWidth <= 0 {
		return displayHeaders, displayRows
	}

	fitWidths := fitColumnWidthsInternal(columnWidths, availableContentWidth)
	for i, header := range displayHeaders {
		displayHeaders[i] = truncateVisibleInternal(header, fitWidths[i])
	}

	for i, row := range displayRows {
		for col := range columnCount {
			row[col] = truncateVisibleInternal(row[col], fitWidths[col])
		}
		displayRows[i] = row
	}

	return displayHeaders, displayRows
}

func sanitizeTableCellInternal(value string) string {
	cleaned := tableWhitespaceReplacer.Replace(value)
	return strings.TrimSpace(cleaned)
}

func visibleWidthInternal(value string) int {
	return runewidth.StringWidth(ansiRegexp.ReplaceAllString(value, ""))
}

func truncateVisibleInternal(value string, maxWidth int) string {
	if maxWidth <= 0 {
		return ""
	}
	if visibleWidthInternal(value) <= maxWidth {
		return value
	}

	plain := ansiRegexp.ReplaceAllString(value, "")
	truncated := "…"
	if maxWidth > 1 {
		truncated = runewidth.Truncate(plain, maxWidth, "…")
	}

	// Re-apply the cell's styling. Colour is written as a single leading escape
	// sequence plus a trailing reset, so measuring against the stripped text and
	// then returning it bare would leave truncated cells uncoloured while their
	// untruncated neighbours in the same column stay tinted.
	codes := ansiRegexp.FindAllString(value, -1)
	if len(codes) == 0 || !strings.HasPrefix(value, codes[0]) {
		return truncated
	}
	return codes[0] + truncated + ansiReset
}

func tableNonContentWidthInternal(columnCount int) int {
	const horizontalCellPadding = 2

	// For normal border style:
	// - one vertical border per boundary: columnCount + 1
	// - one space of left + right padding per cell: 2 * columnCount
	return (columnCount + 1) + (horizontalCellPadding * columnCount)
}

func fitColumnWidthsInternal(widths []int, available int) []int {
	fitted := make([]int, len(widths))
	copy(fitted, widths)

	if len(fitted) == 0 {
		return fitted
	}

	if available < len(fitted) {
		available = len(fitted)
	}

	current := sumIntsInternal(fitted)
	for current > available {
		idx := widestShrinkableColumnInternal(fitted)
		if idx < 0 {
			break
		}
		fitted[idx]--
		current--
	}

	return fitted
}

func widestShrinkableColumnInternal(widths []int) int {
	idx := -1
	maxWidth := 1
	for i, width := range widths {
		if width > maxWidth {
			idx = i
			maxWidth = width
		}
	}
	return idx
}

func sumIntsInternal(values []int) int {
	total := 0
	for _, value := range values {
		total += value
	}
	return total
}

func normalizeTableRowsInternal(rows [][]string, width int) [][]string {
	if width == 0 || len(rows) == 0 {
		return rows
	}

	normalized := make([][]string, len(rows))
	for i, row := range rows {
		cells := make([]string, width)
		copy(cells, row)
		normalized[i] = cells
	}

	return normalized
}

func tintTableRowsInternal(headers []string, rows [][]string) [][]string {
	if len(rows) == 0 {
		return rows
	}
	if !shouldColorInternal() {
		return rows
	}

	result := make([][]string, len(rows))
	for i, row := range rows {
		if len(row) == 0 {
			result[i] = row
			continue
		}
		styled := make([]string, len(row))
		copy(styled, row)
		for col := 0; col < len(row) && col < len(headers); col++ {
			header := strings.ToUpper(strings.TrimSpace(headers[col]))
			switch header {
			case "STATUS", "STATE":
				styled[col] = TintStatus(row[col])
			case "ENABLED":
				styled[col] = TintEnabled(row[col])
			case "IN USE":
				styled[col] = TintYesNo(row[col])
			case "INSECURE":
				styled[col] = TintInsecure(row[col])
			}
		}
		result[i] = styled
	}
	return result
}
