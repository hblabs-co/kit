package banner

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"testing"
)

// stubInterfaceAddrs swaps interfaceAddrs for fn for the duration of
// the test and restores the real net.InterfaceAddrs on cleanup. The
// indirection lets the LAN() branches be exercised regardless of the
// host's actual network configuration (CI runners with only loopback,
// IPv6-only environments, etc).
func stubInterfaceAddrs(t *testing.T, fn func() ([]net.Addr, error)) {
	t.Helper()
	prev := interfaceAddrs
	interfaceAddrs = fn
	t.Cleanup(func() { interfaceAddrs = prev })
}

// TestCenter checks that center pads, balances, and truncates as
// expected. The right-pad gets the extra space when the gap is odd,
// which keeps the visual weight slightly left — that's the look the
// banner relies on, so a regression here would shift every box.
func TestCenter(t *testing.T) {
	cases := []struct {
		in   string
		w    int
		want string
	}{
		{"", 4, "    "},
		{"hi", 4, " hi "},
		{"hey", 6, " hey  "}, // odd gap: right gets the extra space
		{"service-edge", 12, "service-edge"},
		{"way-too-long-name", 5, "way-t"}, // truncated
		{"ñoño", 6, " ñoño "},             // multi-byte runes counted as 1 each
	}
	for _, c := range cases {
		got := center(c.in, c.w)
		if got != c.want {
			t.Errorf("center(%q, %d) = %q, want %q", c.in, c.w, got, c.want)
		}
		if len([]rune(got)) != c.w {
			t.Errorf("center(%q, %d) rune-width = %d, want %d", c.in, c.w, len([]rune(got)), c.w)
		}
	}
}

// TestBox captures stdout to confirm the three box rows render with
// the expected characters and width, the title is centered inside,
// and free-form lines come out indented. Empty entries in `lines`
// must produce a blank line (no indent), so callers can use them as
// separators without leaving dangling spaces.
func TestBox(t *testing.T) {
	out := captureStdout(t, func() {
		Box("svc", "first", "", "second")
	})

	bar := strings.Repeat("─", boxWidth)
	mustContain(t, out, "  ╭"+bar+"╮")
	mustContain(t, out, "  ╰"+bar+"╯")
	mustContain(t, out, "  │"+center("svc", boxWidth)+"│")
	mustContain(t, out, "  first")
	mustContain(t, out, "  second")

	// The blank-separator must be a bare newline, not "  \n".
	if strings.Contains(out, "  \n") {
		t.Errorf("blank line was indented; output:\n%s", out)
	}
}

// TestCard checks the two-column box: borders present, both columns
// rendered, the shorter column padded (vCenter) so every interior line
// is the same visual width, and the empty right-row handled as a blank
// gap. The left block (1 row) is two-plus rows shorter than the right
// (4 rows), so vCenter adds both a top and bottom blank — exercising
// the top-pad loop, the bottom-pad loop, and the right column's
// early-return branch in one call.
func TestCard(t *testing.T) {
	var buf bytes.Buffer
	Card(&buf, []string{"AA"}, []string{"one", "", "three", "four"})
	out := buf.String()

	mustContain(t, out, "╭")
	mustContain(t, out, "╰")
	for _, want := range []string{"AA", "one", "three", "four"} {
		mustContain(t, out, want)
	}

	// Every interior (│ … │) line must share the same rune width, or
	// the right border would zig-zag.
	var widths []int
	for ln := range strings.SplitSeq(out, "\n") {
		if strings.Contains(ln, "│") {
			widths = append(widths, len([]rune(ln)))
		}
	}
	if len(widths) == 0 {
		t.Fatal("no interior lines found")
	}
	for _, wd := range widths[1:] {
		if wd != widths[0] {
			t.Errorf("interior line widths differ: %v", widths)
			break
		}
	}
}

// TestMaxWidthEmpty pins the zero case so the helper's loop-skipped
// branch stays covered independently of Card's usage.
func TestMaxWidthEmpty(t *testing.T) {
	if got := maxWidth(nil); got != 0 {
		t.Errorf("maxWidth(nil) = %d, want 0", got)
	}
}

// TestPrint covers the server banner: the Local URL must always be
// emitted with the requested port, and any extras are appended after
// the URL block. Network is best-effort (depends on host interfaces),
// so we only assert it when LAN() returns something here.
func TestPrint(t *testing.T) {
	out := captureStdout(t, func() {
		Print("service-edge", 8080, "watching: ./pkg")
	})

	mustContain(t, out, "service-edge")
	mustContain(t, out, "➜  Local:    http://localhost:8080")
	mustContain(t, out, "  watching: ./pkg")

	if lan := LAN(); lan != "" {
		mustContain(t, out, fmt.Sprintf("➜  Network:  http://%s:8080", lan))
	}
}

// TestLAN is a smoke test — the result depends on the host's network
// interfaces, so we only assert that the function returns without
// panicking and, if non-empty, gives a parseable IPv4 string.
func TestLAN(t *testing.T) {
	got := LAN()
	if got == "" {
		return
	}
	if strings.Count(got, ".") != 3 {
		t.Errorf("LAN() = %q, want IPv4 dotted-quad or empty", got)
	}
}

// TestLAN_ErrorReturnsEmpty — when net.InterfaceAddrs fails (rare,
// but possible on locked-down containers / weird netns), LAN must
// degrade to "" so callers like Print can simply skip the Network
// line instead of crashing the boot banner.
func TestLAN_ErrorReturnsEmpty(t *testing.T) {
	stubInterfaceAddrs(t, func() ([]net.Addr, error) {
		return nil, errors.New("permission denied")
	})
	if got := LAN(); got != "" {
		t.Errorf("LAN() = %q, want \"\" on InterfaceAddrs error", got)
	}
}

// TestLAN_OnlyLoopbackReturnsEmpty — a host whose only addresses are
// loopback (e.g. CI sandbox, container with no host networking) must
// produce "". Otherwise the banner would advertise 127.0.0.1 as the
// LAN IP, which is misleading on every other device.
func TestLAN_OnlyLoopbackReturnsEmpty(t *testing.T) {
	stubInterfaceAddrs(t, func() ([]net.Addr, error) {
		return []net.Addr{
			&net.IPNet{IP: net.IPv4(127, 0, 0, 1), Mask: net.CIDRMask(8, 32)},
			&net.IPNet{IP: net.ParseIP("::1"), Mask: net.CIDRMask(128, 128)},
		}, nil
	})
	if got := LAN(); got != "" {
		t.Errorf("LAN() = %q, want \"\" with only loopback addresses", got)
	}
}

// TestLAN_OnlyIPv6ReturnsEmpty — non-loopback IPv6-only hosts must
// also produce "". The banner's "Network:" line is hardcoded
// http://<ip>:<port>; an IPv6 literal would need square brackets and
// the URL template doesn't add them, so skipping is the right call.
func TestLAN_OnlyIPv6ReturnsEmpty(t *testing.T) {
	stubInterfaceAddrs(t, func() ([]net.Addr, error) {
		return []net.Addr{
			&net.IPNet{IP: net.ParseIP("fe80::1"), Mask: net.CIDRMask(64, 128)},
			&net.IPNet{IP: net.ParseIP("2001:db8::1"), Mask: net.CIDRMask(64, 128)},
		}, nil
	})
	if got := LAN(); got != "" {
		t.Errorf("LAN() = %q, want \"\" with no IPv4 addresses", got)
	}
}

// TestLAN_FindsFirstIPv4 — happy path with the seam: skip loopback
// + IPv6, return the first non-loopback IPv4 found. Locks in the
// "first wins" semantic so a future refactor doesn't accidentally
// start preferring a later address.
func TestLAN_FindsFirstIPv4(t *testing.T) {
	stubInterfaceAddrs(t, func() ([]net.Addr, error) {
		return []net.Addr{
			&net.IPNet{IP: net.IPv4(127, 0, 0, 1), Mask: net.CIDRMask(8, 32)},
			&net.IPNet{IP: net.ParseIP("fe80::1"), Mask: net.CIDRMask(64, 128)},
			&net.IPNet{IP: net.IPv4(192, 168, 1, 42), Mask: net.CIDRMask(24, 32)},
			&net.IPNet{IP: net.IPv4(10, 0, 0, 5), Mask: net.CIDRMask(8, 32)},
		}, nil
	})
	if got, want := LAN(), "192.168.1.42"; got != want {
		t.Errorf("LAN() = %q, want %q", got, want)
	}
}

// TestLAN_NonIPNetEntrySkipped — InterfaceAddrs may return entries
// that aren't *net.IPNet (e.g. *net.IPAddr in some platforms /
// future stdlib changes). The type-assertion guard must skip those
// silently rather than panic.
func TestLAN_NonIPNetEntrySkipped(t *testing.T) {
	stubInterfaceAddrs(t, func() ([]net.Addr, error) {
		return []net.Addr{
			&net.IPAddr{IP: net.IPv4(192, 168, 1, 99)}, // not *net.IPNet → skipped
			&net.IPNet{IP: net.IPv4(10, 0, 0, 5), Mask: net.CIDRMask(8, 32)},
		}, nil
	})
	if got, want := LAN(), "10.0.0.5"; got != want {
		t.Errorf("LAN() = %q, want %q (non-IPNet should be skipped, not panic)", got, want)
	}
}

// TestPrint_NoLANSkipsNetworkLine — when LAN returns "" Print must
// emit the box + Local URL only, with no "Network:" line. Otherwise
// the banner would either print a half-formed URL or "Network: " with
// nothing after it — both are confusing on a terminal.
func TestPrint_NoLANSkipsNetworkLine(t *testing.T) {
	stubInterfaceAddrs(t, func() ([]net.Addr, error) {
		return nil, errors.New("no networks")
	})

	out := captureStdout(t, func() {
		Print("svc", 8080)
	})

	mustContain(t, out, "➜  Local:    http://localhost:8080")
	if strings.Contains(out, "Network:") {
		t.Errorf("Network line should be absent when LAN()==\"\"; got:\n%s", out)
	}
}

// captureStdout swaps os.Stdout for a pipe, runs fn, and returns
// everything fn printed. The banner package writes through
// fmt.Println (which targets os.Stdout directly), so redirecting at
// the os level is the only way to observe it.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = orig })

	done := make(chan struct{})
	var buf bytes.Buffer
	go func() {
		_, _ = io.Copy(&buf, r)
		close(done)
	}()

	fn()
	_ = w.Close()
	<-done
	return buf.String()
}

func mustContain(t *testing.T, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Errorf("output missing %q\n--- got ---\n%s", needle, haystack)
	}
}
