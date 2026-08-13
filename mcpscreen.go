//go:build windows

package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// defaultScreenshotLongEdge is the fallback cap (in px) for the longer edge of a
// full `screenshot`, chosen to bound per-screenshot vision-token cost. Claude's
// vision downscales anything over ~1568px on the long edge before tokenizing, so an
// uncapped 1080p/4K/multi-monitor grab all cost roughly the same; capping to 1280
// cuts that meaningfully with no practical readability loss. Callers can still
// request full resolution with scale=1.0. Clicking is unaffected — coordinates
// come from win_controls/snapshot.
const defaultScreenshotLongEdge = 1280

// visionLongEdgeCap is the long edge (px) past which Claude's vision downscales an
// image before tokenizing. Pixels beyond it cost no extra vision tokens — they only
// inflate the PNG on the wire and, worse, in the stored transcript, which is replayed
// to the API on every later request. So capture_window/capture_region cap themselves
// here by default instead of returning the raw grab (measured: window captures came
// back 1758x1026 and the vision layer downscaled them to ~1568x915 regardless).
//
// A capped image no longer maps 1:1 to screen pixels, so its caption drops the
// click(left+ix) promise. That promise was already unreliable above this cap for the
// same reason — the model was reading vision-downscaled pixels while being told they
// were raw screen pixels. Callers who genuinely need the raw grab pass scale=1.0.
const visionLongEdgeCap = 1568

// maxScreenshotLongEdge is the active cap, overridable via AGLINK_SCREENSHOT_MAX_EDGE
// (the host sets it from screen_control.max_screenshot_long_edge). A lower value
// trades on-screen text legibility for fewer vision tokens per screenshot. Read once
// at process start; the host spawns a fresh screen MCP process per worker turn, so a
// config change takes effect on the next turn. Clamped so a stray value can't yield a
// 1px or absurdly large grab.
var maxScreenshotLongEdge = resolveScreenshotLongEdge()

// resolveScreenshotLongEdge reads the env override, falling back to the default when
// unset or out of the sane [320, 4096] range.
func resolveScreenshotLongEdge() int {
	if v := os.Getenv("AGLINK_SCREENSHOT_MAX_EDGE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 320 && n <= 4096 {
			return n
		}
	}
	return defaultScreenshotLongEdge
}

// controlCompleteMiddleware wraps every tool handler so that a tool call which
// actually drove the screen is followed by the green "control complete" notice.
// It snapshots syntheticInputCount around the handler: control ops funnel through
// ensureControlNotice (which increments it), while read-only tools leave it
// unchanged, so this cleanly distinguishes "제어" calls from reads with no
// per-tool special-casing. showControlComplete is spawned so the overlay work
// (and any preempt wait) never delays the tool's response.
func controlCompleteMiddleware(next server.ToolHandlerFunc) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		before := syntheticInputCount.Load()
		res, err := next(ctx, req)
		if syntheticInputCount.Load() != before {
			go showControlComplete()
		}
		return res, err
	}
}

// Design Ref: §1 (self-spawned stdio MCP server), §2 (tool table), §4 (mcpscreen.go).
//
// RunMCPScreen runs the "screen" MCP server over stdio (blocking). It is started
// by this binary's "mcp" subcommand (the default when no subcommand is given);
// teleclaude points its claude worker's --mcp-config at this binary so the worker
// connects to the server over stdio.
//
// This is the Windows implementation. Tools start with list_windows and
// focus_window (more added in later tasks: snapshot/screenshot/click/...).
func RunMCPScreen() error {
	// Best-effort safety net: if the worker's turn ends (stdio closes, this
	// process is about to exit) without the LLM ever calling return_desktop,
	// switch back to the desktop the user was on before any cross-desktop
	// focus/capture happened. No-op if no switch occurred. This must not be
	// the ONLY mechanism (the LLM should still call return_desktop once it is
	// done driving the other-desktop window, so the user isn't stranded there
	// mid-turn) but it guarantees the user is never left parked on a desktop
	// they didn't choose just because the model forgot the explicit call.
	defer func() {
		if msg, err := returnToOriginDesktop(); err != nil {
			fmt.Fprintf(os.Stderr, "aglink-screen: warning: return_desktop on exit failed: %v\n", err)
		} else if msg != "" {
			fmt.Fprintf(os.Stderr, "aglink-screen: %s\n", msg)
		}
	}()

	s := server.NewMCPServer(
		"screen",
		"0.1.0",
		server.WithToolCapabilities(true),
		// After any tool call that actually drove the screen, show the green
		// "control complete" ack. The counter advances only for ops that pass
		// through beginSyntheticInput → ensureControlNotice, so read-only tools
		// (screenshot, snapshot, get_value, …) never trigger it. Fire-and-forget
		// so the tool response is never delayed by the overlay.
		server.WithToolHandlerMiddleware(controlCompleteMiddleware),
	)

	// list_windows — visible top-level windows as "TITLE | hwnd=0x..".
	s.AddTool(
		mcp.NewTool("list_windows",
			mcp.WithDescription("List visible top-level windows (across ALL virtual desktops) as 'TITLE | hwnd=0x..'. Windows on another virtual desktop are tagged [other-desktop] — focusing/capturing one switches to its desktop; call return_desktop when done to switch back."),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			wins := enumWindows()
			if len(wins) == 0 {
				return mcp.NewToolResultText("(no visible windows)"), nil
			}
			var b strings.Builder
			for _, w := range wins {
				tag := ""
				if isWindowOnAnotherDesktop(w.HWND) {
					tag = " [other-desktop]"
				}
				fmt.Fprintf(&b, "%s | hwnd=0x%x%s\n", w.Title, w.HWND, tag)
			}
			return mcp.NewToolResultText(strings.TrimRight(b.String(), "\n")), nil
		},
	)

	// launch_app — find and launch an installed app by name.
	s.AddTool(
		mcp.NewTool("launch_app",
			mcp.WithDescription("Launch an installed Windows application by name. Searches Start Menu shortcuts (per-user and machine-wide) for a *.lnk whose name contains the given name, else lets Windows resolve the name (e.g. 'notepad', 'calc'). After launching, give the app a moment to appear, then use focus_window before driving it."),
			mcp.WithString("name",
				mcp.Description("Application name to launch (e.g. 'Calculator', 'Notepad', 'Chrome')."),
				mcp.Required(),
			),
			mcp.WithBoolean("elevated",
				mcp.Description("Launch with administrator rights via a UAC prompt (the user approves it). NOTE: to then click/control the elevated app, teleclaude itself must also be elevated (Windows UIPI). Default false."),
			),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			name, err := req.RequireString("name")
			if err != nil {
				return mcp.NewToolResultError("missing required argument 'name'"), nil
			}
			elevated := req.GetBool("elevated", false)
			desc, err := launchApp(name, elevated)
			if err != nil {
				return mcp.NewToolResultErrorFromErr("launch_app failed", err), nil
			}
			return mcp.NewToolResultText("ok: " + desc), nil
		},
	)

	// focus_window — bring a window to the foreground by title or hwnd.
	s.AddTool(
		mcp.NewTool("focus_window",
			mcp.WithDescription("Bring a window to the foreground. Accepts a window title substring (case-insensitive) or an hwnd like '0x1234'."),
			mcp.WithString("window",
				mcp.Description("Window title substring or hwnd (e.g. '0x1234')."),
				mcp.Required(),
			),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			target, err := req.RequireString("window")
			if err != nil {
				return mcp.NewToolResultError("missing required argument 'window'"), nil
			}
			if err := focusWindow(target); err != nil {
				return mcp.NewToolResultErrorFromErr("focus_window failed", err), nil
			}
			return mcp.NewToolResultText(fmt.Sprintf("ok: focused %q", target)), nil
		},
	)

	// wait_for_window — block until a matching window appears instead of the
	// caller polling list_windows in a loop.
	s.AddTool(
		mcp.NewTool("wait_for_window",
			mcp.WithDescription("Block until a window whose title contains the given substring (or an hwnd) appears, instead of calling list_windows in a manual polling loop. Returns 'TITLE | hwnd=0x..' once found. Fails with a timeout error after 'timeout_ms' (default 8000) if it never appears."),
			mcp.WithString("window", mcp.Description("Window title substring or hwnd to wait for."), mcp.Required()),
			mcp.WithNumber("timeout_ms", mcp.Description("Max time to wait in milliseconds (default 8000).")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			window, err := req.RequireString("window")
			if err != nil {
				return mcp.NewToolResultError("missing required argument 'window'"), nil
			}
			timeoutMs := req.GetInt("timeout_ms", 8000)
			result, err := waitForWindow(window, timeoutMs)
			if err != nil {
				return mcp.NewToolResultErrorFromErr("wait_for_window failed", err), nil
			}
			return mcp.NewToolResultText("ok: " + result), nil
		},
	)

	// wait_for_control — like wait_for_window but for a specific UIA element
	// inside the foreground window (a dialog's control, a panel that renders
	// a moment after the action that triggers it), instead of a manual
	// snapshot-polling loop.
	s.AddTool(
		mcp.NewTool("wait_for_control",
			mcp.WithDescription("Block until an element (by Name or AutomationId from snapshot) appears in the foreground window's UIA tree; times out after 'timeout_ms' (default 8000). Checks tree existence, not visual visibility, so a re-shown WinUI/XAML flyout may return immediately."),
			mcp.WithString("name", mcp.Description("The element Name or AutomationId to wait for."), mcp.Required()),
			mcp.WithNumber("timeout_ms", mcp.Description("Max time to wait in milliseconds (default 8000).")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			name, err := req.RequireString("name")
			if err != nil {
				return mcp.NewToolResultError("missing required argument 'name'"), nil
			}
			timeoutMs := req.GetInt("timeout_ms", 8000)
			result, err := uiaWaitForControl(name, timeoutMs)
			if err != nil {
				return mcp.NewToolResultErrorFromErr("wait_for_control failed", err), nil
			}
			return mcp.NewToolResultText(result), nil
		},
	)

	// move_window — reposition/resize a window precisely (no keyboard-shortcut
	// workarounds like win+left/right needed just to arrange windows on screen).
	s.AddTool(
		mcp.NewTool("move_window",
			mcp.WithDescription("Move and/or resize a window to an exact screen position and size via SetWindowPos. Restores a minimized window first (its iconic placeholder isn't a meaningful target). Useful for arranging two windows side by side before a cross-window drag, or laying out a workspace precisely — an alternative to win+left/right style snap shortcuts when you need exact control."),
			mcp.WithString("window", mcp.Description("Target window: title substring or hwnd."), mcp.Required()),
			mcp.WithNumber("x", mcp.Description("New left position in screen pixels."), mcp.Required()),
			mcp.WithNumber("y", mcp.Description("New top position in screen pixels."), mcp.Required()),
			mcp.WithNumber("width", mcp.Description("New width in pixels (>0)."), mcp.Required()),
			mcp.WithNumber("height", mcp.Description("New height in pixels (>0)."), mcp.Required()),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			window, err := req.RequireString("window")
			if err != nil {
				return mcp.NewToolResultError("missing required argument 'window'"), nil
			}
			x, err := req.RequireInt("x")
			if err != nil {
				return mcp.NewToolResultError("missing required argument 'x'"), nil
			}
			y, err := req.RequireInt("y")
			if err != nil {
				return mcp.NewToolResultError("missing required argument 'y'"), nil
			}
			w, err := req.RequireInt("width")
			if err != nil {
				return mcp.NewToolResultError("missing required argument 'width'"), nil
			}
			h, err := req.RequireInt("height")
			if err != nil {
				return mcp.NewToolResultError("missing required argument 'height'"), nil
			}
			result, err := moveWindow(window, x, y, w, h)
			if err != nil {
				return mcp.NewToolResultErrorFromErr("move_window failed", err), nil
			}
			return mcp.NewToolResultText("ok: " + result), nil
		},
	)

	// window_state — minimize/maximize/restore a window. No "close": that is
	// meaningfully more destructive (unsaved-changes risk) than a state
	// change, and key("alt+f4") already covers it when that's really wanted.
	s.AddTool(
		mcp.NewTool("window_state",
			mcp.WithDescription("Minimize, maximize, or restore a window. Does not close it — use key(\"alt+f4\") for that (more deliberate, since closing can lose unsaved work)."),
			mcp.WithString("window", mcp.Description("Target window: title substring or hwnd."), mcp.Required()),
			mcp.WithString("state", mcp.Description("One of: minimize, maximize, restore."), mcp.Required()),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			window, err := req.RequireString("window")
			if err != nil {
				return mcp.NewToolResultError("missing required argument 'window'"), nil
			}
			state, err := req.RequireString("state")
			if err != nil {
				return mcp.NewToolResultError("missing required argument 'state'"), nil
			}
			result, err := setWindowState(window, state)
			if err != nil {
				return mcp.NewToolResultErrorFromErr("window_state failed", err), nil
			}
			return mcp.NewToolResultText("ok: " + result), nil
		},
	)

	// get_window_rect — read-side counterpart to move_window/window_state, so
	// a window's current position/size/state can be checked instead of
	// guessed from a screenshot (e.g. to verify a move_window call landed, or
	// to decide how to move a window before doing so).
	s.AddTool(
		mcp.NewTool("get_window_rect",
			mcp.WithDescription("Get a window's current screen position, size, and minimize/maximize/normal state. The read-side counterpart to move_window/window_state — use this to verify a move_window call landed, or to check current placement before deciding how to move a window."),
			mcp.WithString("window", mcp.Description("Target window: title substring or hwnd."), mcp.Required()),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			window, err := req.RequireString("window")
			if err != nil {
				return mcp.NewToolResultError("missing required argument 'window'"), nil
			}
			result, err := getWindowRectInfo(window)
			if err != nil {
				return mcp.NewToolResultErrorFromErr("get_window_rect failed", err), nil
			}
			return mcp.NewToolResultText(result), nil
		},
	)

	// close_window — close a SPECIFIC window by title/hwnd (WM_CLOSE), unlike
	// key("alt+f4") which acts on whatever the OS currently considers
	// foreground. Prefer this whenever the target window is known, since
	// foreground can shift unexpectedly (e.g. launch_app on an already-running
	// single-instance app activates its existing window instead of opening a
	// new one) — ambiguous alt+f4 targeting has caused real data loss.
	s.AddTool(
		mcp.NewTool("close_window",
			mcp.WithDescription("Close a specific window by title substring or hwnd (WM_CLOSE — the app's own \"save changes?\" prompt still fires; follow with confirm_dialogs to auto-handle it). Prefer over key(\"alt+f4\") when you know the target: alt+f4 hits whatever is currently foreground, this targets the exact window by handle."),
			mcp.WithString("window", mcp.Description("Target window: title substring or hwnd."), mcp.Required()),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			window, err := req.RequireString("window")
			if err != nil {
				return mcp.NewToolResultError("missing required argument 'window'"), nil
			}
			result, err := closeWindow(window)
			if err != nil {
				return mcp.NewToolResultErrorFromErr("close_window failed", err), nil
			}
			return mcp.NewToolResultText("ok: sent close to " + result), nil
		},
	)

	// return_desktop — switch back to the virtual desktop that was active before
	// the screen tools first jumped to another desktop to operate a window there.
	s.AddTool(
		mcp.NewTool("return_desktop",
			mcp.WithDescription("Switch back to the virtual desktop that was active before you focused/captured a window on another desktop. Call this when finished operating on an [other-desktop] window so the user is returned to where they were. No-op if no cross-desktop switch happened."),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			msg, err := returnToOriginDesktop()
			if err != nil {
				return mcp.NewToolResultErrorFromErr("return_desktop failed", err), nil
			}
			return mcp.NewToolResultText("ok: " + msg), nil
		},
	)

	// screenshot — capture the full virtual screen and return it as an image so
	// Claude's vision can read it. By DEFAULT the image is downscaled so its longer
	// edge is at most maxScreenshotLongEdge px, which bounds the per-screenshot
	// vision-token cost (Claude already downscales anything over ~1568px, so an
	// uncapped grab costs the same ~1800 tokens regardless of monitor size). Pass
	// scale=1.0 to force full resolution, or an explicit 0.1–1.0 factor.
	s.AddTool(
		mcp.NewTool("screenshot",
			mcp.WithDescription(fmt.Sprintf("Capture the entire screen and return it as a PNG image. Use this to see what is currently on screen. To SAVE TOKENS the image is downscaled by default to at most %dpx on its longer edge (readable, and clicking is unaffected — click coordinates come from win_controls/snapshot, not this image). Pass scale=1.0 for full resolution, or an explicit 0.1–1.0 downscale factor.", maxScreenshotLongEdge)),
			mcp.WithNumber("scale",
				mcp.Description("Optional downscale factor between 0.1 and 1.0. Omit for the default token-saving downscale; pass 1.0 for full resolution."),
			),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			// scale omitted → default capped capture (token-bounded). scale given →
			// honor it exactly (1.0 = full resolution). GetFloat returns 0 when the
			// arg is absent, which we treat as "use the default cap".
			scale := req.GetFloat("scale", 0)
			if scale != 0 && (scale < 0.1 || scale > 1.0) {
				return mcp.NewToolResultError("scale must be between 0.1 and 1.0"), nil
			}
			var (
				png []byte
				err error
			)
			if scale == 0 {
				png, err = captureScreenCapped(maxScreenshotLongEdge)
			} else {
				png, err = captureScreenScaled(scale)
			}
			if err != nil {
				return mcp.NewToolResultErrorFromErr("screenshot failed", err), nil
			}
			b64 := base64.StdEncoding.EncodeToString(png)
			return mcp.NewToolResultImage("Screenshot of the current screen.", b64, "image/png"), nil
		},
	)

	// capture_window — crop the screenshot to one window. Smaller image stays
	// under the vision downscale cap, so it is sharp and pixel-accurate; the
	// returned caption gives the origin to map image pixels to screen coords.
	s.AddTool(
		mcp.NewTool("capture_window",
			mcp.WithDescription(fmt.Sprintf("Capture ONLY the given window (cropped to its rectangle) as a PNG. Prefer this over the full screenshot. A window under %dpx on its longer edge comes back untouched — sharp, and its pixels map exactly to screen coordinates, so the caption gives the origin and an in-image pixel (ix,iy) maps to click(x=left+ix, y=top+iy). A bigger window is capped to %dpx (the vision layer would downscale it anyway); the caption then says so and the click mapping does NOT hold. To READ content even more cheaply, pass scale (0.1–1.0) — reading only, do not compute clicks from it. Even a capped window still costs ~1800 vision tokens and is re-sent on every later request, so reach for snapshot/get_text first.", visionLongEdgeCap, visionLongEdgeCap)),
			mcp.WithString("window", mcp.Description("Target window: title substring or hwnd."), mcp.Required()),
			mcp.WithNumber("scale", mcp.Description(fmt.Sprintf("Optional downscale factor 0.1–1.0 for READING only (fewer vision tokens). Omit for the default %dpx long-edge cap; pass 1.0 to force the raw grab with exact click mapping.", visionLongEdgeCap))),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			window, err := req.RequireString("window")
			if err != nil {
				return mcp.NewToolResultError("missing required argument 'window'"), nil
			}
			scale := req.GetFloat("scale", 0)
			if scale != 0 && (scale < 0.1 || scale > 1.0) {
				return mcp.NewToolResultError("scale must be between 0.1 and 1.0"), nil
			}
			png, left, top, w, h, err := captureWindow(window)
			if err != nil {
				return mcp.NewToolResultErrorFromErr("capture_window failed", err), nil
			}
			// Full resolution (default): keep the exact 1:1 image→screen click mapping.
			caption := fmt.Sprintf("Window %q. Screen origin (left=%d, top=%d), size %dx%d. "+
				"To click an element at image pixel (ix,iy), call click(x=%d+ix, y=%d+iy).",
				window, left, top, w, h, left, top)
			switch {
			case scale != 0 && scale < 1.0:
				var iw, ih int
				png, iw, ih, err = scaleReadingPNG(png, scale)
				if err != nil {
					return mcp.NewToolResultErrorFromErr("capture_window downscale failed", err), nil
				}
				// Downscaled: the 1:1 mapping no longer holds, so steer clicks to the
				// reliable path instead of offering error-prone scaled arithmetic.
				caption = fmt.Sprintf("Window %q, downscaled to %dx%d for reading (from %dx%d). "+
					"Read-only: do NOT compute click coordinates from this image — use snapshot/win_controls, "+
					"or re-capture without scale for pixel-accurate clicking.", window, iw, ih, w, h)
			case scale == 0:
				// Default: cap the long edge. scale=1.0 skips this and returns the raw grab.
				var iw, ih int
				var capped bool
				png, iw, ih, capped, err = capVisionLongEdge(png)
				if err != nil {
					return mcp.NewToolResultErrorFromErr("capture_window cap failed", err), nil
				}
				if capped {
					caption = fmt.Sprintf("Window %q, capped to %dx%d (from %dx%d) — past %dpx the vision layer "+
						"downscales anyway, so the extra pixels only cost transcript bytes. "+
						"The 1:1 click mapping does NOT hold here: take coordinates from snapshot/win_controls, "+
						"capture_region a sub-rectangle for an exact-mapping image, or pass scale=1.0 for the raw grab.",
						window, iw, ih, w, h, visionLongEdgeCap)
				}
			}
			b64 := base64.StdEncoding.EncodeToString(png)
			return mcp.NewToolResultImage(caption, b64, "image/png"), nil
		},
	)

	// capture_region — capture an arbitrary rectangle as a PNG. Coordinates are
	// absolute screen pixels, or relative to a window's top-left when 'window' is
	// given (which also switches to that window's virtual desktop first).
	s.AddTool(
		mcp.NewTool("capture_region",
			mcp.WithDescription(fmt.Sprintf("Capture an arbitrary rectangle as a PNG — the cheapest way to look at just the part you need. (x,y) is the top-left; width/height the size. (x,y) are ABSOLUTE screen pixels, or RELATIVE to 'window' when given (switches to its desktop first). A rectangle under %dpx on its longer edge is returned untouched and the caption reports the absolute origin, so image pixel (ix,iy) maps to click(x=origin+ix, y=origin+iy); a bigger one is capped to %dpx and loses that mapping (the caption says so). Pass scale (0.1–1.0) to downscale further for READING only, or scale=1.0 to force the raw grab.", visionLongEdgeCap, visionLongEdgeCap)),
			mcp.WithNumber("x", mcp.Description("Left of the rectangle: absolute screen X, or window-relative X if 'window' is set."), mcp.Required()),
			mcp.WithNumber("y", mcp.Description("Top of the rectangle: absolute screen Y, or window-relative Y if 'window' is set."), mcp.Required()),
			mcp.WithNumber("width", mcp.Description("Rectangle width in pixels (>0)."), mcp.Required()),
			mcp.WithNumber("height", mcp.Description("Rectangle height in pixels (>0)."), mcp.Required()),
			mcp.WithString("window", mcp.Description("Optional target window (title substring or hwnd). When set, x/y are relative to this window's top-left.")),
			mcp.WithNumber("scale", mcp.Description(fmt.Sprintf("Optional downscale factor 0.1–1.0 for READING only (fewer vision tokens). Omit for the default %dpx long-edge cap; pass 1.0 to force the raw grab with exact click mapping.", visionLongEdgeCap))),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			x, err := req.RequireInt("x")
			if err != nil {
				return mcp.NewToolResultError("missing required argument 'x'"), nil
			}
			y, err := req.RequireInt("y")
			if err != nil {
				return mcp.NewToolResultError("missing required argument 'y'"), nil
			}
			w, err := req.RequireInt("width")
			if err != nil {
				return mcp.NewToolResultError("missing required argument 'width'"), nil
			}
			h, err := req.RequireInt("height")
			if err != nil {
				return mcp.NewToolResultError("missing required argument 'height'"), nil
			}
			window := req.GetString("window", "")
			scale := req.GetFloat("scale", 0)
			if scale != 0 && (scale < 0.1 || scale > 1.0) {
				return mcp.NewToolResultError("scale must be between 0.1 and 1.0"), nil
			}
			png, absX, absY, err := captureRegionAt(window, x, y, w, h)
			if err != nil {
				return mcp.NewToolResultErrorFromErr("capture_region failed", err), nil
			}
			var caption string
			if strings.TrimSpace(window) != "" {
				caption = fmt.Sprintf("Region of window %q at window-relative (%d,%d), size %dx%d — screen origin (%d,%d). "+
					"To click image pixel (ix,iy), call click(x=%d+ix, y=%d+iy).", window, x, y, w, h, absX, absY, absX, absY)
			} else {
				caption = fmt.Sprintf("Screen region at (%d,%d), size %dx%d. "+
					"To click image pixel (ix,iy), call click(x=%d+ix, y=%d+iy).", absX, absY, w, h, absX, absY)
			}
			// Downscaled: same rule as capture_window — the 1:1 image→screen mapping is
			// gone, so send clicks back through the reliable path instead of scaled math.
			switch {
			case scale != 0 && scale < 1.0:
				var iw, ih int
				png, iw, ih, err = scaleReadingPNG(png, scale)
				if err != nil {
					return mcp.NewToolResultErrorFromErr("capture_region downscale failed", err), nil
				}
				caption = fmt.Sprintf("Region at screen origin (%d,%d), downscaled to %dx%d for reading (from %dx%d). "+
					"Read-only: do NOT compute click coordinates from this image — use snapshot/win_controls, "+
					"or re-capture without scale for pixel-accurate clicking.", absX, absY, iw, ih, w, h)
			case scale == 0:
				// Default: cap the long edge. scale=1.0 skips this and returns the raw grab.
				var iw, ih int
				var capped bool
				png, iw, ih, capped, err = capVisionLongEdge(png)
				if err != nil {
					return mcp.NewToolResultErrorFromErr("capture_region cap failed", err), nil
				}
				if capped {
					caption = fmt.Sprintf("Region at screen origin (%d,%d), capped to %dx%d (from %dx%d) — past %dpx the "+
						"vision layer downscales anyway, so the extra pixels only cost transcript bytes. "+
						"The 1:1 click mapping does NOT hold here: take coordinates from snapshot/win_controls, "+
						"re-capture a smaller rectangle for an exact-mapping image, or pass scale=1.0 for the raw grab.",
						absX, absY, iw, ih, w, h, visionLongEdgeCap)
				}
			}
			b64 := base64.StdEncoding.EncodeToString(png)
			return mcp.NewToolResultImage(caption, b64, "image/png"), nil
		},
	)

	// control_status — report cross-process screen-control ownership (read-only).
	// Lets a caller check, BEFORE driving the screen, whether another concurrent
	// teleclaude session currently holds control — the proactive counterpart to the
	// SCREEN_BUSY error a control op returns when contended. See
	// docs/control-ownership.md §4.2. Does not acquire the lease.
	s.AddTool(
		mcp.NewTool("control_status",
			mcp.WithDescription("Report who holds screen-control ownership across concurrent sessions: 'control: free' / 'control: held by me (...)' / 'control: held by another (...)'. Read-only, does NOT take control. Check before driving the screen to avoid a SCREEN_BUSY collision (a click/type would otherwise return that error)."),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText(controlStatusText()), nil
		},
	)

	// ---- Input tools (mouse / keyboard / scroll) ----

	// click — move to (x,y) and click a mouse button, optionally holding modifiers.
	s.AddTool(
		mcp.NewTool("click",
			mcp.WithDescription("Move the mouse to absolute screen pixel (x,y) and click. button is left (default), right, or middle. Optional 'modifiers' holds keys during the click for multi-select, e.g. 'ctrl' or 'ctrl+shift' (ctrl-click / shift-click)."),
			mcp.WithNumber("x", mcp.Description("Absolute X pixel on the virtual desktop."), mcp.Required()),
			mcp.WithNumber("y", mcp.Description("Absolute Y pixel on the virtual desktop."), mcp.Required()),
			mcp.WithString("button", mcp.Description("Mouse button: left (default), right, or middle.")),
			mcp.WithString("modifiers", mcp.Description("Optional modifier keys held during the click, '+'-separated: ctrl, alt, shift, win. e.g. 'ctrl' or 'ctrl+shift'.")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			x, err := req.RequireInt("x")
			if err != nil {
				return mcp.NewToolResultError("missing required argument 'x'"), nil
			}
			y, err := req.RequireInt("y")
			if err != nil {
				return mcp.NewToolResultError("missing required argument 'y'"), nil
			}
			button := req.GetString("button", "left")
			modStr := strings.TrimSpace(req.GetString("modifiers", ""))
			if modStr != "" {
				if err := mouseClickMods(x, y, button, strings.Split(modStr, "+")); err != nil {
					return mcp.NewToolResultErrorFromErr("click failed", err), nil
				}
				return mcp.NewToolResultText(fmt.Sprintf("ok: %s+%s-clicked at (%d,%d)", modStr, button, x, y)), nil
			}
			if err := mouseClick(x, y, button); err != nil {
				return mcp.NewToolResultErrorFromErr("click failed", err), nil
			}
			return mcp.NewToolResultText(fmt.Sprintf("ok: %s-clicked at (%d,%d)", button, x, y)), nil
		},
	)

	// drag — press at (x1,y1), move to (x2,y2), release (rubber-band/slider/drag-drop).
	s.AddTool(
		mcp.NewTool("drag",
			mcp.WithDescription("Press a mouse button at (x1,y1), drag to (x2,y2) through intermediate steps, then release. Use for rubber-band multi-select, sliders, drag & drop, and text selection. button is left (default), right, or middle."),
			mcp.WithNumber("x1", mcp.Description("Start X pixel."), mcp.Required()),
			mcp.WithNumber("y1", mcp.Description("Start Y pixel."), mcp.Required()),
			mcp.WithNumber("x2", mcp.Description("End X pixel."), mcp.Required()),
			mcp.WithNumber("y2", mcp.Description("End Y pixel."), mcp.Required()),
			mcp.WithString("button", mcp.Description("Mouse button: left (default), right, or middle.")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			x1, err := req.RequireInt("x1")
			if err != nil {
				return mcp.NewToolResultError("missing required argument 'x1'"), nil
			}
			y1, err := req.RequireInt("y1")
			if err != nil {
				return mcp.NewToolResultError("missing required argument 'y1'"), nil
			}
			x2, err := req.RequireInt("x2")
			if err != nil {
				return mcp.NewToolResultError("missing required argument 'x2'"), nil
			}
			y2, err := req.RequireInt("y2")
			if err != nil {
				return mcp.NewToolResultError("missing required argument 'y2'"), nil
			}
			button := req.GetString("button", "left")
			if err := mouseDrag(x1, y1, x2, y2, button); err != nil {
				return mcp.NewToolResultErrorFromErr("drag failed", err), nil
			}
			return mcp.NewToolResultText(fmt.Sprintf("ok: %s-dragged (%d,%d) -> (%d,%d)", button, x1, y1, x2, y2)), nil
		},
	)

	// move — move the mouse cursor without clicking.
	s.AddTool(
		mcp.NewTool("move",
			mcp.WithDescription("Move the mouse cursor to absolute screen pixel (x,y) without clicking."),
			mcp.WithNumber("x", mcp.Description("Absolute X pixel on the virtual desktop."), mcp.Required()),
			mcp.WithNumber("y", mcp.Description("Absolute Y pixel on the virtual desktop."), mcp.Required()),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			x, err := req.RequireInt("x")
			if err != nil {
				return mcp.NewToolResultError("missing required argument 'x'"), nil
			}
			y, err := req.RequireInt("y")
			if err != nil {
				return mcp.NewToolResultError("missing required argument 'y'"), nil
			}
			if err := mouseMove(x, y); err != nil {
				return mcp.NewToolResultErrorFromErr("move failed", err), nil
			}
			return mcp.NewToolResultText(fmt.Sprintf("ok: moved to (%d,%d)", x, y)), nil
		},
	)

	// get_cursor_position — read where the mouse currently is. Matches
	// Anthropic's own computer-use reference tool, which offers this
	// alongside move/click — useful to verify a move()/drag() actually
	// landed, or as a base point for a relative next move.
	s.AddTool(
		mcp.NewTool("get_cursor_position",
			mcp.WithDescription("Get the mouse cursor's current absolute screen position (x,y). Useful to confirm move()/drag() landed where expected, or as a reference point for a relative next move."),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			x, y := cursorPos()
			return mcp.NewToolResultText(fmt.Sprintf("(%d,%d)", x, y)), nil
		},
	)

	// double_click — left double-click at (x,y).
	s.AddTool(
		mcp.NewTool("double_click",
			mcp.WithDescription("Left double-click at absolute screen pixel (x,y)."),
			mcp.WithNumber("x", mcp.Description("Absolute X pixel on the virtual desktop."), mcp.Required()),
			mcp.WithNumber("y", mcp.Description("Absolute Y pixel on the virtual desktop."), mcp.Required()),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			x, err := req.RequireInt("x")
			if err != nil {
				return mcp.NewToolResultError("missing required argument 'x'"), nil
			}
			y, err := req.RequireInt("y")
			if err != nil {
				return mcp.NewToolResultError("missing required argument 'y'"), nil
			}
			if err := mouseDouble(x, y); err != nil {
				return mcp.NewToolResultErrorFromErr("double_click failed", err), nil
			}
			return mcp.NewToolResultText(fmt.Sprintf("ok: double-clicked at (%d,%d)", x, y)), nil
		},
	)

	// triple_click — left triple-click, selects a whole line/paragraph in most
	// text editors and word processors (a gesture double_click doesn't cover).
	s.AddTool(
		mcp.NewTool("triple_click",
			mcp.WithDescription("Left triple-click at absolute screen pixel (x,y) — selects a whole line or paragraph in most text editors/word processors."),
			mcp.WithNumber("x", mcp.Description("Absolute X pixel on the virtual desktop."), mcp.Required()),
			mcp.WithNumber("y", mcp.Description("Absolute Y pixel on the virtual desktop."), mcp.Required()),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			x, err := req.RequireInt("x")
			if err != nil {
				return mcp.NewToolResultError("missing required argument 'x'"), nil
			}
			y, err := req.RequireInt("y")
			if err != nil {
				return mcp.NewToolResultError("missing required argument 'y'"), nil
			}
			if err := mouseTriple(x, y); err != nil {
				return mcp.NewToolResultErrorFromErr("triple_click failed", err), nil
			}
			return mcp.NewToolResultText(fmt.Sprintf("ok: triple-clicked at (%d,%d)", x, y)), nil
		},
	)

	// type — type a Unicode string at the current focus.
	s.AddTool(
		mcp.NewTool("type",
			mcp.WithDescription("Type a Unicode text string into the currently focused control (per-character key events). Does not press Enter."),
			mcp.WithString("text", mcp.Description("The text to type."), mcp.Required()),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			text, err := req.RequireString("text")
			if err != nil {
				return mcp.NewToolResultError("missing required argument 'text'"), nil
			}
			if err := typeText(text); err != nil {
				return mcp.NewToolResultErrorFromErr("type failed", err), nil
			}
			return mcp.NewToolResultText(fmt.Sprintf("ok: typed %d characters", len([]rune(text)))), nil
		},
	)

	// key — press a key combo (e.g. "ctrl+c", "alt+f4", "enter").
	s.AddTool(
		mcp.NewTool("key",
			mcp.WithDescription("Press a key or key combo such as 'enter', 'ctrl+c', 'alt+f4', 'ctrl+shift+s'. Modifiers: ctrl, alt, shift, win. Also supports system media keys, which work regardless of which window is focused: 'volumeup', 'volumedown', 'volumemute'/'mute', 'medianext', 'mediaprev', 'mediastop', 'mediaplay'/'playpause', 'printscreen'/'prtsc'. Optional 'hold_ms' holds the key down for that many milliseconds before releasing, instead of an instant tap — for UIs/games that distinguish a long-press."),
			mcp.WithString("combo", mcp.Description("Key combo, e.g. 'ctrl+c' or 'enter'."), mcp.Required()),
			mcp.WithNumber("hold_ms", mcp.Description("Hold the key down this many milliseconds before releasing (default: instant tap).")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			combo, err := req.RequireString("combo")
			if err != nil {
				return mcp.NewToolResultError("missing required argument 'combo'"), nil
			}
			holdMs := req.GetInt("hold_ms", 0)
			if err := keyComboHold(combo, holdMs); err != nil {
				return mcp.NewToolResultErrorFromErr("key failed", err), nil
			}
			if holdMs > 0 {
				return mcp.NewToolResultText(fmt.Sprintf("ok: held %q for %dms", combo, holdMs)), nil
			}
			return mcp.NewToolResultText(fmt.Sprintf("ok: pressed %q", combo)), nil
		},
	)

	// scroll — scroll the mouse wheel. dy up/down, dx left/right (in lines).
	s.AddTool(
		mcp.NewTool("scroll",
			mcp.WithDescription("Scroll the mouse wheel. dy>0 scrolls up, dy<0 down; dx>0 right, dx<0 left. Units are wheel notches/lines."),
			mcp.WithNumber("dx", mcp.Description("Horizontal scroll amount (lines). Positive = right.")),
			mcp.WithNumber("dy", mcp.Description("Vertical scroll amount (lines). Positive = up.")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			dx := req.GetInt("dx", 0)
			dy := req.GetInt("dy", 0)
			if dx == 0 && dy == 0 {
				return mcp.NewToolResultError("scroll requires a non-zero dx or dy"), nil
			}
			if err := scroll(dx, dy); err != nil {
				return mcp.NewToolResultErrorFromErr("scroll failed", err), nil
			}
			return mcp.NewToolResultText(fmt.Sprintf("ok: scrolled dx=%d dy=%d%s", dx, dy, uipiWarning(windowUnderCursor(), "scroll"))), nil
		},
	)

	// ---- Coordinate preset tools ----

	presetPath, err := defaultPresetsPath()
	if err != nil {
		return fmt.Errorf("presets path: %w", err)
	}
	presets := NewPresetStore(presetPath)
	if err := presets.Load(); err != nil {
		// A corrupt/unreadable presets file must not brick the whole screen
		// server (screenshot/click/type/... are the primary function; presets
		// are secondary). Warn and start with an empty store; a later
		// preset_save heals the file. Load leaves the store empty on failure.
		fmt.Fprintf(os.Stderr, "aglink-screen: warning: could not load presets from %s: %v (starting with empty preset store)\n", presetPath, err)
	}

	// preset_save — store a named (x,y) coordinate.
	s.AddTool(
		mcp.NewTool("preset_save",
			mcp.WithDescription("Save a named screen coordinate so it can be clicked later by name with preset_click."),
			mcp.WithString("name", mcp.Description("Preset name."), mcp.Required()),
			mcp.WithNumber("x", mcp.Description("Absolute X pixel."), mcp.Required()),
			mcp.WithNumber("y", mcp.Description("Absolute Y pixel."), mcp.Required()),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			name, err := req.RequireString("name")
			if err != nil {
				return mcp.NewToolResultError("missing required argument 'name'"), nil
			}
			x, err := req.RequireInt("x")
			if err != nil {
				return mcp.NewToolResultError("missing required argument 'x'"), nil
			}
			y, err := req.RequireInt("y")
			if err != nil {
				return mcp.NewToolResultError("missing required argument 'y'"), nil
			}
			if err := presets.Set(name, x, y); err != nil {
				return mcp.NewToolResultErrorFromErr("preset_save failed", err), nil
			}
			return mcp.NewToolResultText(fmt.Sprintf("ok: saved preset %q = (%d,%d)", name, x, y)), nil
		},
	)

	// preset_click — left-click a previously saved preset coordinate.
	s.AddTool(
		mcp.NewTool("preset_click",
			mcp.WithDescription("Left-click a previously saved coordinate preset by name."),
			mcp.WithString("name", mcp.Description("Preset name to click."), mcp.Required()),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			name, err := req.RequireString("name")
			if err != nil {
				return mcp.NewToolResultError("missing required argument 'name'"), nil
			}
			p, ok := presets.Get(name)
			if !ok {
				return mcp.NewToolResultError(fmt.Sprintf("no preset named %q", name)), nil
			}
			if err := mouseClick(p.X, p.Y, "left"); err != nil {
				return mcp.NewToolResultErrorFromErr("preset_click failed", err), nil
			}
			return mcp.NewToolResultText(fmt.Sprintf("ok: clicked preset %q at (%d,%d)", name, p.X, p.Y)), nil
		},
	)

	// preset_list — list all saved presets.
	s.AddTool(
		mcp.NewTool("preset_list",
			mcp.WithDescription("List all saved coordinate presets as 'name | x,y' lines."),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			list := presets.List()
			if len(list) == 0 {
				return mcp.NewToolResultText("(no presets saved)"), nil
			}
			var b strings.Builder
			for _, p := range list {
				fmt.Fprintf(&b, "%s | %d,%d\n", p.Name, p.X, p.Y)
			}
			return mcp.NewToolResultText(strings.TrimRight(b.String(), "\n")), nil
		},
	)

	// ---- Win32 child-window controls (works when UIA is empty) ----

	// win_controls — enumerate a window's real Win32 child controls with exact
	// screen coordinates. Use this when snapshot (UIA) returns nothing but the
	// app is native (buttons/tree/list are real child windows). Cheaper and far
	// more reliable than screenshot+vision for clicking by label.
	s.AddTool(
		mcp.NewTool("win_controls",
			mcp.WithDescription("List a window's Win32 child controls with EXACT screen coordinates: 'class | \"label\" | center(x,y) | WxH'. Use the reported center as click(x,y), or click_control to click by label. Works for native apps even when snapshot/UIA is empty. Only visible controls unless include_hidden=true. Large trees are capped by 'max' (default 400)."),
			mcp.WithString("window", mcp.Description("Target window: title substring or hwnd (e.g. 'NetGuard')."), mcp.Required()),
			mcp.WithBoolean("include_hidden", mcp.Description("Include controls that are not currently visible (other tabs/panels). Default false.")),
			mcp.WithNumber("max", mcp.Description("Max controls to return (default 400); a big list/tree is truncated with a note. Symmetry with snapshot's cap so a huge tree can't flood the prompt.")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			window, err := req.RequireString("window")
			if err != nil {
				return mcp.NewToolResultError("missing required argument 'window'"), nil
			}
			includeHidden := req.GetBool("include_hidden", false)
			maxControls := req.GetInt("max", 400)
			ctrls, err := listControls(window, includeHidden)
			if err != nil {
				return mcp.NewToolResultErrorFromErr("win_controls failed", err), nil
			}
			if len(ctrls) == 0 {
				return mcp.NewToolResultText("(no child controls found)"), nil
			}
			// Cap the count so a SysListView32/SysTreeView32 with thousands of rows
			// can't dump its whole tree into the prompt. snapshot already caps its
			// element count; this closes the same gap for win_controls.
			note := ""
			if maxControls > 0 && len(ctrls) > maxControls {
				note = fmt.Sprintf("\n… [%d of %d controls shown; raise 'max' or narrow the window]", maxControls, len(ctrls))
				ctrls = ctrls[:maxControls]
			}
			var b strings.Builder
			for _, c := range ctrls {
				vis := ""
				if !c.Visible {
					vis = " [hidden]"
				}
				fmt.Fprintf(&b, "%s | %q | center(%d,%d) | %dx%d%s\n",
					c.Class, c.Text, c.CenterX(), c.CenterY(),
					c.Right-c.Left, c.Bottom-c.Top, vis)
			}
			return mcp.NewToolResultText(strings.TrimRight(b.String(), "\n") + note), nil
		},
	)

	// click_control — click a child control by its label (exact coords, no vision).
	s.AddTool(
		mcp.NewTool("click_control",
			mcp.WithDescription("Find a visible Win32 child control by its label text (case-insensitive substring) in the given window and left-click its center. The window is focused first. If multiple controls share the label, use 'nth' (0-based) to pick which one — list them first with win_controls. Preferred over click(x,y) for native apps."),
			mcp.WithString("window", mcp.Description("Target window: title substring or hwnd."), mcp.Required()),
			mcp.WithString("text", mcp.Description("Control label to match (e.g. '로컬장치 검색', 'File')."), mcp.Required()),
			mcp.WithNumber("nth", mcp.Description("0-based index when several controls share the label (default 0).")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			window, err := req.RequireString("window")
			if err != nil {
				return mcp.NewToolResultError("missing required argument 'window'"), nil
			}
			text, err := req.RequireString("text")
			if err != nil {
				return mcp.NewToolResultError("missing required argument 'text'"), nil
			}
			nth := req.GetInt("nth", 0)
			desc, err := clickControl(window, text, nth)
			if err != nil {
				return mcp.NewToolResultErrorFromErr("click_control failed", err), nil
			}
			return mcp.NewToolResultText("ok: " + desc), nil
		},
	)

	// confirm_dialogs — auto-click affirmative buttons on the app's confirmation
	// popups so an automated sweep runs continuously without human input.
	s.AddTool(
		mcp.NewTool("confirm_dialogs",
			mcp.WithDescription("Auto-handle confirmation dialogs that an app pops up (e.g. '전송하시겠습니까?' / 'OK?'). Detects up to 'max' consecutive dialogs — separate popup windows owned by the app, or a dialog rendered as child controls of the main window — and clicks an affirmative button on each. Returns one line per dialog handled (empty if none). Use right after a command click so a feature sweep proceeds without stopping for the user."),
			mcp.WithString("app", mcp.Description("Target app window: title substring or hwnd."), mcp.Required()),
			mcp.WithString("accept", mcp.Description("Optional '+'-separated affirmative button labels to match (case-insensitive substring). Default: 예+확인+Yes+OK+전송+보내+Send+Apply+적용.")),
			mcp.WithNumber("max", mcp.Description("Max consecutive dialogs to handle (default 5).")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			app, err := req.RequireString("app")
			if err != nil {
				return mcp.NewToolResultError("missing required argument 'app'"), nil
			}
			var accept []string
			if a := strings.TrimSpace(req.GetString("accept", "")); a != "" {
				accept = strings.Split(a, "+")
			}
			maxN := req.GetInt("max", 5)
			handled, err := confirmDialogs(app, accept, maxN)
			if err != nil {
				return mcp.NewToolResultErrorFromErr("confirm_dialogs failed", err), nil
			}
			if len(handled) == 0 {
				return mcp.NewToolResultText("ok: no dialogs detected"), nil
			}
			return mcp.NewToolResultText("ok: handled " + fmt.Sprintf("%d dialog(s):\n", len(handled)) + strings.Join(handled, "\n")), nil
		},
	)

	// ---- UIA (UI Automation) tools — preferred over screenshot/click ----

	// snapshot — read the foreground window's UIA element tree as text.
	s.AddTool(
		mcp.NewTool("snapshot",
			mcp.WithDescription("Read the foreground window's UI Automation element tree as compact text: control type, name, automation id, capabilities ([invokable]/[editable]/[text]/[disabled]), and a short inlined content preview (= \"…\") for fields/editors that carry text. Prefer this over a screenshot — it is cheap and reliable for native apps and shows both structure AND a content preview in one call. For the FULL text of an element or document, follow up with get_text (or get_value for a single field). Optional 'max' caps the number of elements (default 200)."),
			mcp.WithNumber("max",
				mcp.Description("Maximum number of elements to return (default 200)."),
			),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			max := req.GetInt("max", 200)
			text, err := uiaSnapshot(max)
			if err != nil {
				return mcp.NewToolResultErrorFromErr("snapshot failed", err), nil
			}
			return mcp.NewToolResultText(text), nil
		},
	)

	// element_at — resolve the UIA element under a screen point. The bridge
	// between "I saw something in a screenshot" and "here is its accessible
	// name" — spot a target visually, then act on it via invoke/set_value/
	// click_control instead of a raw coordinate click.
	s.AddTool(
		mcp.NewTool("element_at",
			mcp.WithDescription("Resolve the UI Automation element at an absolute screen point (x,y) — same info as one snapshot line: control type, name, automation id, capabilities. Use this after spotting something in a screenshot but before acting on it: get its name here, then drive it with invoke/set_value/click_control (far more reliable than a raw coordinate click, which can silently land on the wrong control)."),
			mcp.WithNumber("x", mcp.Description("Absolute screen X pixel."), mcp.Required()),
			mcp.WithNumber("y", mcp.Description("Absolute screen Y pixel."), mcp.Required()),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			x, err := req.RequireInt("x")
			if err != nil {
				return mcp.NewToolResultError("missing required argument 'x'"), nil
			}
			y, err := req.RequireInt("y")
			if err != nil {
				return mcp.NewToolResultError("missing required argument 'y'"), nil
			}
			text, err := uiaElementAtPoint(int32(x), int32(y))
			if err != nil {
				return mcp.NewToolResultErrorFromErr("element_at failed", err), nil
			}
			return mcp.NewToolResultText(text), nil
		},
	)

	// invoke — activate an element found by name (or automation id).
	s.AddTool(
		mcp.NewTool("invoke",
			mcp.WithDescription("Find an element by its Name (or AutomationId) in the foreground window and activate it (button/menu item/tree item/checkbox). Use a name reported by snapshot. Preferred over click(x,y)."),
			mcp.WithString("name",
				mcp.Description("The element Name or AutomationId to invoke."),
				mcp.Required(),
			),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			name, err := req.RequireString("name")
			if err != nil {
				return mcp.NewToolResultError("missing required argument 'name'"), nil
			}
			if err := uiaInvoke(name); err != nil {
				return mcp.NewToolResultErrorFromErr("invoke failed", err), nil
			}
			return mcp.NewToolResultText(fmt.Sprintf("ok: invoked %q", name)), nil
		},
	)

	// set_value — set the text of an editable element found by name.
	s.AddTool(
		mcp.NewTool("set_value",
			mcp.WithDescription("Find an editable element by its Name (or AutomationId) in the foreground window and set its text via the UIA Value pattern. Preferred over click+type for known input fields."),
			mcp.WithString("name",
				mcp.Description("The element Name or AutomationId of the input field."),
				mcp.Required(),
			),
			mcp.WithString("text",
				mcp.Description("The text to set."),
				mcp.Required(),
			),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			name, err := req.RequireString("name")
			if err != nil {
				return mcp.NewToolResultError("missing required argument 'name'"), nil
			}
			text, err := req.RequireString("text")
			if err != nil {
				return mcp.NewToolResultError("missing required argument 'text'"), nil
			}
			if err := uiaSetValue(name, text); err != nil {
				return mcp.NewToolResultErrorFromErr("set_value failed", err), nil
			}
			return mcp.NewToolResultText(fmt.Sprintf("ok: set %q = %q", name, text)), nil
		},
	)

	// get_value — read an editable element's CURRENT text via the UIA Value
	// pattern. The read-side counterpart to set_value: useful for confirming
	// what a field actually holds now (autocomplete may have rewritten it, a
	// calculated field may have updated, etc.) rather than guessing from
	// snapshot's truncated label.
	s.AddTool(
		mcp.NewTool("get_value",
			mcp.WithDescription("Find an editable element by its Name (or AutomationId) in the foreground window and read its CURRENT text via the UIA Value pattern. The read-side counterpart to set_value — use this to confirm what a field actually holds now (e.g. after autocomplete rewrote it, or a calculated field updated) instead of guessing from snapshot's truncated label."),
			mcp.WithString("name",
				mcp.Description("The element Name or AutomationId of the input field."),
				mcp.Required(),
			),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			name, err := req.RequireString("name")
			if err != nil {
				return mcp.NewToolResultError("missing required argument 'name'"), nil
			}
			value, err := uiaGetValue(name)
			if err != nil {
				return mcp.NewToolResultErrorFromErr("get_value failed", err), nil
			}
			return mcp.NewToolResultText(fmt.Sprintf("%q = %q", name, value)), nil
		},
	)

	// get_text — read a control's (or the whole foreground window's) textual
	// content by handle, so a worker reads documents/editors/read-only panes as
	// TEXT instead of capturing them as a screenshot (10–100× cheaper, exact, and
	// not re-billed every turn as a retained image). Uses the UIA Text pattern with
	// a Value fallback — the read path for content-heavy controls that snapshot
	// only previews and get_value (Value-only) used to reject.
	s.AddTool(
		mcp.NewTool("get_text",
			mcp.WithDescription(fmt.Sprintf("Read a window/control's full TEXT content by handle (UIA Text pattern, Value fallback) — use this to READ a document, editor, log pane, article, or read-only text area instead of a screenshot. Give 'name' (element Name/AutomationId from snapshot) to read one element; omit it to read the foreground window's document text. Returns up to %d chars (bounded). Prefer this and snapshot over capture_window for reading text; capture only for genuinely visual content.", maxTextChars)),
			mcp.WithString("name",
				mcp.Description("Optional element Name/AutomationId to read. Omit to read the foreground window's own text content."),
			),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			name := req.GetString("name", "")
			text, err := uiaGetText(name)
			if err != nil {
				return mcp.NewToolResultErrorFromErr("get_text failed", err), nil
			}
			if strings.TrimSpace(text) == "" {
				return mcp.NewToolResultText("(no text content)"), nil
			}
			return mcp.NewToolResultText(text), nil
		},
	)

	// ---- batch execution ----

	// run_sequence — execute several actions in one call instead of one
	// round-trip per step. The bulk of a screen-control turn's wall-clock time
	// is the reasoning between tool calls, not the calls themselves, so this
	// is the highest-leverage way to cut that down for any sequence whose
	// targets are already known (e.g. from a prior snapshot) — fill a form,
	// a known multi-click flow, etc. It deliberately does NOT try to be
	// "smart": no embedded model, no branching. It stops at the first failed
	// step and reports how far it got, so a sequence never silently partially
	// applies — genuinely unpredictable steps (reacting to a popup that may
	// or may not appear) belong outside the batch, split at that point.
	s.AddTool(
		mcp.NewTool("run_sequence",
			mcp.WithDescription(`Execute a batch of screen actions in one call instead of one round-trip per step. Actions (same params as each own tool): click{x,y,button?,modifiers?}, double_click{x,y}, triple_click{x,y}, type{text}, key{combo,hold_ms?}, invoke{name}, set_value{name,text}, click_control{window,text,nth?}, wait_for_control{name,timeout_ms?}, wait_for_window{window,timeout_ms?}, scroll{dx?,dy?}, drag{x,y,x2,y2,button?}. Stops at the first failed step and reports how far it got. Use only for steps whose targets you already know; keep any final destructive/committing action (send, delete, confirm) out of the batch and issue it separately after verifying state.`),
			mcp.WithString("steps", mcp.Description(`JSON array of step objects, each with an "action" field plus that action's params. Example: [{"action":"click","x":100,"y":200},{"action":"type","text":"hello"},{"action":"key","combo":"tab"}]`), mcp.Required()),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			stepsJSON, err := req.RequireString("steps")
			if err != nil {
				return mcp.NewToolResultError("missing required argument 'steps'"), nil
			}
			results, rerr := runSequence(stepsJSON)
			// Compact (not indented) JSON: this is machine-read step output, so the
			// pretty-print newlines/spaces were pure token overhead every call.
			b, _ := json.Marshal(results)
			if rerr != nil {
				return mcp.NewToolResultError(fmt.Sprintf("run_sequence stopped early: %v\n%s", rerr, string(b))), nil
			}
			return mcp.NewToolResultText(fmt.Sprintf("ok: %d step(s) completed\n%s", len(results), string(b))), nil
		},
	)

	return server.ServeStdio(s)
}
