// Package banner prints a service startup banner with local + LAN
// URLs. Used by every service to give a consistent first
// impression in the terminal: name centered in a box, "Local:" and
// "Network:" URLs side by side so a phone on the same wifi can hit
// the LAN IP without copy-pasting.
//
//	╭──────────────────────────────────────────────────╮
//	│                   service-edge                   │
//	╰──────────────────────────────────────────────────╯
//	➜  Local:    http://localhost:8080
//	➜  Network:  http://192.168.1.42:8080
//
// The optional `extras` argument accepts any number of trailing
// lines (e.g. a list of watched directories for the dev server) —
// each is printed verbatim after the URLs, indented with two spaces.
package banner

import (
	"fmt"
	"io"
	"net"
	"os"
	"strings"
)

// boxWidth is the inner width of the banner box. 50 chars is wide
// enough for "service-something" centered with breathing room, and
// matches the look-and-feel the rest of the platform uses.
const boxWidth = 50

// Print writes a server startup banner: the box with the service
// name, plus Local / Network URLs derived from the listen port.
//
//   - name   the service title shown centered (e.g. "service-edge")
//   - port   listen port; both Local + Network URLs use it
//   - extras optional lines printed below the URLs, verbatim with
//     a two-space indent (caller decides bullets / spacing)
//
// For non-server output (CLI tools, scripts) use Box instead, which
// draws the same box but skips the port / URL lines.
func Print(name string, port int, extras ...string) {
	lines := []string{
		fmt.Sprintf("➜  Local:    http://localhost:%d", port),
	}
	if lan := LAN(); lan != "" {
		lines = append(lines, fmt.Sprintf("➜  Network:  http://%s:%d", lan, port))
	}

	lines = append(lines, fmt.Sprintf("➜  PID:      %d", os.Getpid()))
	lines = append(lines, extras...)
	Box(name, lines...)
}

// Box prints just the centered-title box, optionally followed by
// any number of free-form lines. CLI tools reuse the box style
// without dragging in port / URL concepts that don't apply to a
// one-shot command.
//
// Each line in `lines` is printed verbatim after a two-space
// indent so it lines up under the box. Pass an empty string for
// blank-line separators.
//
// Box targets stdout; BoxTo is the writer-aware variant for callers
// (CLI tools, tests) that need to capture or redirect the output.
func Box(title string, lines ...string) {
	BoxTo(os.Stdout, title, lines...)
}

// BoxTo writes the centered-title box to w, followed by any free-form
// lines. It is the same drawing Box performs; Box is just the stdout
// convenience wrapper. Exposed so shared CLI scaffolding
// (service-tools/internal/clitool) can render the platform banner to a
// captured writer and stay testable.
func BoxTo(w io.Writer, title string, lines ...string) {
	bar := strings.Repeat("─", boxWidth)
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  ╭"+bar+"╮")
	fmt.Fprintln(w, "  │"+center(title, boxWidth)+"│")
	fmt.Fprintln(w, "  ╰"+bar+"╯")
	for _, ln := range lines {
		if ln == "" {
			fmt.Fprintln(w)
			continue
		}
		fmt.Fprintln(w, "  "+ln)
	}
	fmt.Fprintln(w)
}

// cardGap is the column of spaces between the left art block and the
// right text block in Card. Three spaces reads as a deliberate gutter
// without pushing the text too far from the logo.
const cardGap = "   "

// Card draws a fully-enclosed two-column box: a left art block (e.g. an
// ASCII logo) beside a right text block, like a business card. Unlike
// Box / Frame, the box width is computed from the content rather than
// the fixed boxWidth, so the card grows to fit the longest line. The
// shorter column is vertically centered against the taller one; pass an
// empty string in `right` for an intentional blank row (e.g. between a
// brand line and a property list).
//
// Used by service-tools/internal/clitool for the HB Labs CLI welcome
// card (logo left, name/version/mail/web right).
func Card(w io.Writer, left, right []string) {
	leftW := maxWidth(left)
	rightW := maxWidth(right)
	rows := max(len(left), len(right))
	left = vCenter(left, rows)
	right = vCenter(right, rows)

	inner := 1 + leftW + len(cardGap) + rightW + 1
	bar := strings.Repeat("─", inner)
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  ╭"+bar+"╮")
	for i := range rows {
		line := " " + padRight(left[i], leftW) + cardGap + padRight(right[i], rightW) + " "
		fmt.Fprintln(w, "  │"+line+"│")
	}
	fmt.Fprintln(w, "  ╰"+bar+"╯")
	fmt.Fprintln(w)
}

// maxWidth returns the widest line in runes (not bytes, so the █ blocks
// of an ASCII logo count as one column each, matching how a monospace
// terminal renders them).
func maxWidth(lines []string) int {
	m := 0
	for _, l := range lines {
		if w := len([]rune(l)); w > m {
			m = w
		}
	}
	return m
}

// padRight right-pads s with spaces to exactly width runes. A no-op
// when s already fills the width (the art block's longest lines), which
// keeps every Card row the same visual width so the right border lines
// up.
func padRight(s string, width int) string {
	n := width - len([]rune(s))
	if n <= 0 {
		return s
	}
	return s + strings.Repeat(" ", n)
}

// vCenter pads lines to exactly rows entries by adding blank lines,
// split above and below so a shorter column sits centered against the
// taller one. Returns lines unchanged when it already has enough rows.
func vCenter(lines []string, rows int) []string {
	if len(lines) >= rows {
		return lines
	}
	top := (rows - len(lines)) / 2
	out := make([]string, 0, rows)
	for range top {
		out = append(out, "")
	}
	out = append(out, lines...)
	for len(out) < rows {
		out = append(out, "")
	}
	return out
}

// interfaceAddrs is the seam tests use to drive LAN's branches
// (error path, only-loopback, only-IPv6) deterministically. Swap via
// t.Cleanup; production never touches this var.
var interfaceAddrs = net.InterfaceAddrs

// LAN returns the first non-loopback IPv4 address on any interface,
// or "" if none was found. Exported so callers can log the LAN IP
// separately if Print's full banner isn't appropriate.
func LAN() string {
	addrs, err := interfaceAddrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok || ipn.IP.IsLoopback() {
			continue
		}
		if ip4 := ipn.IP.To4(); ip4 != nil {
			return ip4.String()
		}
	}
	return ""
}

// center pads s with equal-ish spaces so it occupies exactly w
// runes inside the box. Truncates if s is too long (rare — service
// names are short by convention).
func center(s string, w int) string {
	runes := []rune(s)
	if len(runes) >= w {
		return string(runes[:w])
	}
	pad := w - len(runes)
	left := pad / 2
	right := pad - left
	return strings.Repeat(" ", left) + s + strings.Repeat(" ", right)
}
