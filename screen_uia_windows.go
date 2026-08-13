//go:build windows

package main

import (
	"fmt"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	ole "github.com/go-ole/go-ole"
	"golang.org/x/sys/windows"
)

// Design Ref: §2 (snapshot / invoke / set_value), §4 (screen_uia_windows.go).
//
// CGO-free Windows UI Automation (UIA) access for the "screen" MCP server.
//
// We talk to IUIAutomation (a pure COM / IUnknown-based interface, NOT IDispatch)
// by calling its vtable methods directly via syscall on the interface pointers.
// go-ole is used only for COM init, CoCreateInstance, and BSTR helpers.
//
// All COM calls must run on a single, OS-locked STA thread. We dedicate one
// goroutine (uiaWorker) that initializes COM once and serves every UIA request
// over a channel, so the MCP handlers (which run on arbitrary goroutines) never
// touch COM directly.

// ---- Well-known UIA GUIDs / IDs (from UIAutomationClient.h) ----

// CLSID_CUIAutomation {ff48dba4-60ef-4201-aa87-54103eef594e}
var clsidCUIAutomation = ole.NewGUID("{ff48dba4-60ef-4201-aa87-54103eef594e}")

// IID_IUIAutomation {30cbe57d-d9d0-452a-ab13-7ac5ac4825ee}
var iidIUIAutomation = ole.NewGUID("{30cbe57d-d9d0-452a-ab13-7ac5ac4825ee}")

const (
	// Property IDs.
	uiaNamePropertyId         = 30005
	uiaAutomationIdPropertyId = 30011

	// Control pattern IDs.
	uiaInvokePatternId         = 10000
	uiaValuePatternId          = 10002
	uiaTextPatternId           = 10014
	uiaTogglePatternId         = 10015
	uiaSelectionItemPatternId  = 10010
	uiaExpandCollapsePatternId = 10005

	// TreeScope flags.
	treeScopeElement     = 1
	treeScopeChildren    = 2
	treeScopeDescendants = 4
	treeScopeSubtree     = 7
)

// ---- IUIAutomation vtable slot indices (incl. IUnknown 0,1,2) ----
const (
	uiaGetRootElement          = 5 // GetRootElement(out **IUIAutomationElement)
	uiaElementFromHandle       = 6 // ElementFromHandle(HWND, out **IUIAutomationElement)
	uiaElementFromPoint        = 7 // ElementFromPoint(POINT, out **IUIAutomationElement)
	uiaGetFocusedElement       = 8 // GetFocusedElement(out **IUIAutomationElement)
	uiaCreateTrueCondition     = 21
	uiaCreatePropertyCondition = 23 // CreatePropertyCondition(PROPERTYID, VARIANT, out **IUIAutomationCondition)
)

// ---- IUIAutomationElement vtable slot indices (incl. IUnknown 0,1,2) ----
const (
	elemSetFocus               = 3
	elemFindFirst              = 5  // FindFirst(scope, *cond, out **elem)
	elemFindAll                = 6  // FindAll(scope, *cond, out **IUIAutomationElementArray)
	elemGetCurrentPattern      = 16 // GetCurrentPattern(patternId, out **IUnknown)
	elemGetCurrentControlType  = 21 // -> *int32 (CONTROLTYPEID)
	elemGetCurrentName         = 23 // -> *BSTR
	elemGetCurrentIsEnabled    = 28 // -> *BOOL(int32)
	elemGetCurrentAutomationId = 29 // -> *BSTR
)

// ---- IUIAutomationElementArray vtable slot indices ----
const (
	arrGetLength  = 3 // get_Length(out *int32)
	arrGetElement = 4 // GetElement(int32 index, out **elem)
)

// ---- Pattern vtable slots (all inherit IUnknown 0,1,2) ----
const (
	invokePatternInvoke      = 3 // Invoke()
	valuePatternSetValue     = 3 // SetValue(BSTR)
	valuePatternCurrentValue = 4 // get_CurrentValue(BSTR*) — verified against
	// Microsoft Learn's IUIAutomationValuePattern vtable order: SetValue,
	// get_CurrentValue, get_CurrentIsReadOnly, get_CachedValue, get_CachedIsReadOnly.
	togglePatternToggle  = 3 // Toggle()
	selectionItemSelect  = 3 // Select()
	expandCollapseExpand = 3 // Expand()

	// IUIAutomationTextPattern: get_DocumentRange returns a TextRange spanning the
	// whole control; IUIAutomationTextRange.GetText(maxLength, *BSTR) reads it.
	// This is how Documents / read-only text areas / editors expose their content
	// (they have no Value pattern), so it's the read path for "what does this say".
	textPatternGetDocumentRange = 7  // get_DocumentRange(out **IUIAutomationTextRange)
	textRangeGetText            = 12 // GetText(int maxLength, out *BSTR)
)

// controlTypeName maps a UIA control-type id to a short readable label.
func controlTypeName(id int32) string {
	switch id {
	case 50000:
		return "button"
	case 50001:
		return "calendar"
	case 50002:
		return "checkbox"
	case 50003:
		return "combobox"
	case 50004:
		return "edit"
	case 50005:
		return "hyperlink"
	case 50006:
		return "image"
	case 50007:
		return "list item"
	case 50008:
		return "list"
	case 50009:
		return "menu"
	case 50010:
		return "menu bar"
	case 50011:
		return "menu item"
	case 50012:
		return "progress bar"
	case 50013:
		return "radio button"
	case 50014:
		return "scroll bar"
	case 50015:
		return "slider"
	case 50016:
		return "spinner"
	case 50017:
		return "status bar"
	case 50018:
		return "tab"
	case 50019:
		return "tab item"
	case 50020:
		return "text"
	case 50021:
		return "tool bar"
	case 50022:
		return "tooltip"
	case 50023:
		return "tree"
	case 50024:
		return "tree item"
	case 50025:
		return "custom"
	case 50026:
		return "group"
	case 50027:
		return "thumb"
	case 50028:
		return "data grid"
	case 50029:
		return "data item"
	case 50030:
		return "document"
	case 50031:
		return "split button"
	case 50032:
		return "window"
	case 50033:
		return "pane"
	case 50034:
		return "header"
	case 50035:
		return "header item"
	case 50036:
		return "table"
	case 50037:
		return "title bar"
	case 50038:
		return "separator"
	case 50039:
		return "semantic zoom"
	case 50040:
		return "app bar"
	default:
		return fmt.Sprintf("type%d", id)
	}
}

// ---- Low-level vtable call helper ----

// vcall invokes vtable slot `slot` of the COM object `this` with the given
// uintptr args and returns the HRESULT.
func vcall(this *ole.IUnknown, slot int, args ...uintptr) uintptr {
	if this == nil {
		return uintptr(0x80004003) // E_POINTER
	}
	// this.RawVTable points at the start of the vtable (array of fn pointers).
	// View it as a large fixed array and index the requested slot. We go
	// straight from the RawVTable pointer (no uintptr round-trip) so the
	// vet "misuse of unsafe.Pointer" heuristic stays quiet.
	vtbl := (*[256]uintptr)(unsafe.Pointer(this.RawVTable))
	fn := vtbl[slot]
	all := append([]uintptr{uintptr(unsafe.Pointer(this))}, args...)
	ret, _, _ := syscall.SyscallN(fn, all...)
	return ret
}

func failed(hr uintptr) bool { return int32(hr) < 0 }

// addRefRelease helpers via IUnknown vtable.
func release(u *ole.IUnknown) {
	if u != nil {
		vcall(u, 2)
	}
}

// ---- UIA worker goroutine (single STA thread) ----

type uiaRequest struct {
	fn    func(*ole.IUnknown) (string, error) // receives the live IUIAutomation
	reply chan uiaResult
}

type uiaResult struct {
	text string
	err  error
}

var (
	uiaOnce    sync.Once
	uiaReqCh   chan uiaRequest
	uiaInitErr error
)

// startUIAWorker spins up the dedicated STA COM thread once.
func startUIAWorker() {
	uiaReqCh = make(chan uiaRequest)
	ready := make(chan struct{})
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		// COINIT_APARTMENTTHREADED == 0x2.
		if err := ole.CoInitializeEx(0, 0x2); err != nil {
			uiaInitErr = fmt.Errorf("CoInitializeEx: %w", err)
			close(ready)
			return
		}
		defer ole.CoUninitialize()

		unk, err := ole.CreateInstance(clsidCUIAutomation, iidIUIAutomation)
		if err != nil {
			uiaInitErr = fmt.Errorf("create CUIAutomation: %w", err)
			close(ready)
			return
		}
		defer release(unk)
		close(ready)

		for req := range uiaReqCh {
			text, e := req.fn(unk)
			req.reply <- uiaResult{text: text, err: e}
		}
	}()
	<-ready
}

// uiaDo runs fn on the UIA STA thread and returns its result.
func uiaDo(fn func(*ole.IUnknown) (string, error)) (string, error) {
	uiaOnce.Do(startUIAWorker)
	if uiaInitErr != nil {
		return "", uiaInitErr
	}
	reply := make(chan uiaResult, 1)
	uiaReqCh <- uiaRequest{fn: fn, reply: reply}
	r := <-reply
	return r.text, r.err
}

// ---- Element helpers (run on the STA thread) ----

// foregroundElement returns the IUIAutomationElement for the foreground window,
// or falls back to the root (desktop) element if there is no foreground window.
func foregroundElement(uia *ole.IUnknown) (*ole.IUnknown, error) {
	hwnd, _, _ := procGetForegroundWindow.Call()
	if hwnd != 0 {
		var elem *ole.IUnknown
		hr := vcall(uia, uiaElementFromHandle, hwnd, uintptr(unsafe.Pointer(&elem)))
		if !failed(hr) && elem != nil {
			return elem, nil
		}
	}
	var root *ole.IUnknown
	hr := vcall(uia, uiaGetRootElement, uintptr(unsafe.Pointer(&root)))
	if failed(hr) || root == nil {
		return nil, fmt.Errorf("UIA: no foreground window and GetRootElement failed (hr=0x%x)", uint32(hr))
	}
	return root, nil
}

// packPoint packs a POINT{LONG x; LONG y} into the single 64-bit register it
// is passed in by value on amd64: x in the low 32 bits, y in the high 32.
// Extracted so the packing — the one non-obvious bit, and the one that must
// survive negative coordinates (monitors left of / above the primary sit at
// negative virtual-screen coords) — is unit-testable without a live COM call.
// uint32(int32) round-trips the sign so Windows reads the LONG back correctly.
func packPoint(x, y int32) uintptr {
	return uintptr(uint32(x)) | uintptr(uint32(y))<<32
}

// elementFromPoint returns the UIA element at absolute screen coordinates
// (x,y), the bridge between "I saw something at this pixel in a screenshot"
// and an actual accessibility element. Caller must release the result.
func elementFromPoint(uia *ole.IUnknown, x, y int32) (*ole.IUnknown, error) {
	var elem *ole.IUnknown
	hr := vcall(uia, uiaElementFromPoint, packPoint(x, y), uintptr(unsafe.Pointer(&elem)))
	if failed(hr) || elem == nil {
		return nil, fmt.Errorf("UIA: ElementFromPoint(%d,%d) failed (hr=0x%x) — point may be off-screen or over no element", x, y, uint32(hr))
	}
	return elem, nil
}

// elemString reads a BSTR-returning property (Name / AutomationId).
func elemString(elem *ole.IUnknown, slot int) string {
	var bstr *uint16
	hr := vcall(elem, slot, uintptr(unsafe.Pointer(&bstr)))
	if failed(hr) || bstr == nil {
		return ""
	}
	s := ole.BstrToString(bstr)
	ole.SysFreeString((*int16)(unsafe.Pointer(bstr)))
	return s
}

// elemInt32 reads an int32-returning property (ControlType, IsEnabled).
func elemInt32(elem *ole.IUnknown, slot int) int32 {
	var v int32
	vcall(elem, slot, uintptr(unsafe.Pointer(&v)))
	return v
}

// elemSupportsPattern returns true if the element exposes the given pattern.
// The returned *IUnknown (if any) is released; callers that need the pattern
// should use getPattern instead.
func elemSupportsPattern(elem *ole.IUnknown, patternId int) bool {
	p := getPattern(elem, patternId)
	if p != nil {
		release(p)
		return true
	}
	return false
}

// getPattern returns the pattern interface for patternId, or nil if unsupported.
func getPattern(elem *ole.IUnknown, patternId int) *ole.IUnknown {
	var pat *ole.IUnknown
	hr := vcall(elem, elemGetCurrentPattern, uintptr(patternId), uintptr(unsafe.Pointer(&pat)))
	if failed(hr) || pat == nil {
		return nil
	}
	return pat
}

// ---- snapshot ----

type uiaNode struct {
	name     string
	ctrlType int32
	autoId   string
	enabled  bool
	invoke   bool
	value    bool
	text     bool   // exposes the Text pattern (Document/read-only text area)
	preview  string // short inlined content preview (see snapshotPreviewChars)
	depth    int
}

// snapshotPreviewChars bounds the per-element content preview inlined into a
// snapshot line. Short on purpose: enough to see WHAT a field/editor holds
// without a screenshot or a get_value round-trip, but small so it can't bloat the
// tree — the full content is one get_value/get_text call away.
const snapshotPreviewChars = 120

// onelinePreview flattens a value/text preview to a single trimmed line capped at
// max runes (with an ellipsis), so an inlined snapshot preview stays one grep-able
// line no matter how the source wraps.
func onelinePreview(s string, max int) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\t", " ")
	s = strings.Join(strings.Fields(s), " ") // collapse runs of whitespace
	r := []rune(s)
	if len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}

// uiaSnapshot walks the foreground window's element subtree (children-first,
// breadth-limited) and returns a compact textual listing capped at maxElems.
// uiaSnapshotSparseThreshold is the FindAll element count below which
// uiaSnapshot suspects the target hasn't finished building its accessibility
// tree yet and retries once. Chromium/Electron apps (VS Code, Chrome) often
// expose an empty or near-empty tree on the very first UIA query after gaining
// focus — the query itself seems to be what triggers them to start building
// it — and a second query a moment later sees the real tree. Native Win32/WPF
// apps return hundreds of elements immediately, so this path essentially never
// fires for them and costs nothing.
const uiaSnapshotSparseThreshold = 10

// uiaSnapshotRetryDelay is how long uiaSnapshot waits before its one retry.
var uiaSnapshotRetryDelay = 250 * time.Millisecond

func uiaSnapshot(maxElems int) (string, error) {
	if maxElems <= 0 {
		maxElems = 200
	}
	return uiaDo(func(uia *ole.IUnknown) (string, error) {
		root, err := foregroundElement(uia)
		if err != nil {
			return "", err
		}
		defer release(root)

		// TrueCondition matches every element.
		var cond *ole.IUnknown
		hr := vcall(uia, uiaCreateTrueCondition, uintptr(unsafe.Pointer(&cond)))
		if failed(hr) || cond == nil {
			return "", fmt.Errorf("UIA: CreateTrueCondition failed (hr=0x%x)", uint32(hr))
		}
		defer release(cond)

		// FindAll over the subtree (descendants + self). findAll is a closure so
		// it can be called a second time (with a fresh COM array each time) if the
		// first pass looks suspiciously sparse.
		findAll := func() (*ole.IUnknown, int32, error) {
			var arr *ole.IUnknown
			hr := vcall(root, elemFindAll, uintptr(treeScopeSubtree),
				uintptr(unsafe.Pointer(cond)), uintptr(unsafe.Pointer(&arr)))
			if failed(hr) || arr == nil {
				return nil, 0, fmt.Errorf("UIA: FindAll failed (hr=0x%x)", uint32(hr))
			}
			var l int32
			vcall(arr, arrGetLength, uintptr(unsafe.Pointer(&l)))
			return arr, l, nil
		}

		arr, length, err := findAll()
		if err != nil {
			return "", err
		}
		if length < uiaSnapshotSparseThreshold {
			time.Sleep(uiaSnapshotRetryDelay)
			if arr2, length2, err2 := findAll(); err2 == nil && length2 > length {
				release(arr)
				arr, length = arr2, length2
			} else if arr2 != nil {
				release(arr2) // retry didn't help; keep the first (possibly still-empty) result
			}
		}
		defer release(arr)

		truncated := false
		n := int(length)
		if n > maxElems {
			n = maxElems
			truncated = true
		}

		var nodes []uiaNode
		for i := 0; i < n; i++ {
			var el *ole.IUnknown
			hr := vcall(arr, arrGetElement, uintptr(int32(i)), uintptr(unsafe.Pointer(&el)))
			if failed(hr) || el == nil {
				continue
			}
			name := elemString(el, elemGetCurrentName)
			ct := elemInt32(el, elemGetCurrentControlType)
			autoId := elemString(el, elemGetCurrentAutomationId)
			enabled := elemInt32(el, elemGetCurrentIsEnabled) != 0
			canInvoke := elemSupportsPattern(el, uiaInvokePatternId)
			canValue := elemSupportsPattern(el, uiaValuePatternId)
			canText := elemSupportsPattern(el, uiaTextPatternId)
			// Inline a short content preview so one snapshot shows structure AND
			// content — the model can read a form's field values / an editor's text
			// without a screenshot or a get_value per field. Value first (edit/combo),
			// then Text (documents/read-only areas). Bounded hard; full content is a
			// get_value/get_text away. Read before release(el).
			preview := ""
			if canValue {
				if s, ok := elemValue(el); ok {
					preview = s
				}
			}
			if preview == "" && canText {
				if s, ok := elemText(el, snapshotPreviewChars*2); ok {
					preview = s
				}
			}
			release(el)

			// Skip wholly anonymous, non-interactive, contentless nodes to save tokens.
			if strings.TrimSpace(name) == "" && autoId == "" && !canInvoke && !canValue && !canText {
				continue
			}
			nodes = append(nodes, uiaNode{
				name:     name,
				ctrlType: ct,
				autoId:   autoId,
				enabled:  enabled,
				invoke:   canInvoke,
				value:    canValue,
				text:     canText,
				preview:  onelinePreview(preview, snapshotPreviewChars),
			})
		}

		// Stable, readable order: by control type then name.
		sort.SliceStable(nodes, func(i, j int) bool {
			if nodes[i].ctrlType != nodes[j].ctrlType {
				return nodes[i].ctrlType < nodes[j].ctrlType
			}
			return nodes[i].name < nodes[j].name
		})

		var b strings.Builder
		fmt.Fprintf(&b, "UIA elements of foreground window (%d shown of %d):\n", len(nodes), length)
		for _, nd := range nodes {
			caps := ""
			if nd.invoke {
				caps += " [invokable]"
			}
			if nd.value {
				caps += " [editable]"
			}
			if nd.text {
				caps += " [text]"
			}
			if !nd.enabled {
				caps += " [disabled]"
			}
			name := nd.name
			if name == "" {
				name = "(unnamed)"
			}
			fmt.Fprintf(&b, "- %s | %q", controlTypeName(nd.ctrlType), name)
			if nd.autoId != "" {
				fmt.Fprintf(&b, " | id=%s", nd.autoId)
			}
			b.WriteString(caps)
			if nd.preview != "" {
				fmt.Fprintf(&b, " = %q", nd.preview)
			}
			b.WriteString("\n")
		}
		if truncated {
			fmt.Fprintf(&b, "(truncated to %d elements; increase 'max' to see more)\n", maxElems)
		}
		return strings.TrimRight(b.String(), "\n"), nil
	})
}

// uiaElementAtPoint resolves the UIA element under absolute screen point
// (x,y) and describes it the same way a snapshot line does. This is the
// missing link between screenshot-based visual identification and reliable
// UIA-based action: spot something in a screenshot, get its accessible name
// here, then drive it with invoke/set_value/click_control instead of a raw
// coordinate click.
func uiaElementAtPoint(x, y int32) (string, error) {
	return uiaDo(func(uia *ole.IUnknown) (string, error) {
		el, err := elementFromPoint(uia, x, y)
		if err != nil {
			return "", err
		}
		defer release(el)

		name := elemString(el, elemGetCurrentName)
		ct := elemInt32(el, elemGetCurrentControlType)
		autoId := elemString(el, elemGetCurrentAutomationId)
		enabled := elemInt32(el, elemGetCurrentIsEnabled) != 0
		canInvoke := elemSupportsPattern(el, uiaInvokePatternId)
		canValue := elemSupportsPattern(el, uiaValuePatternId)

		caps := ""
		if canInvoke {
			caps += " [invokable]"
		}
		if canValue {
			caps += " [editable]"
		}
		if !enabled {
			caps += " [disabled]"
		}
		displayName := name
		if displayName == "" {
			displayName = "(unnamed)"
		}

		var b strings.Builder
		fmt.Fprintf(&b, "%s | %q", controlTypeName(ct), displayName)
		if autoId != "" {
			fmt.Fprintf(&b, " | id=%s", autoId)
		}
		b.WriteString(caps)
		if name != "" {
			fmt.Fprintf(&b, "\nUse invoke/set_value/click_control with name %q to act on it (prefer name over automationId if both are set).", name)
		} else if autoId != "" {
			fmt.Fprintf(&b, "\nNo accessible name, but has an automationId — invoke/set_value also match against automationId, so try %q there.", autoId)
		} else {
			b.WriteString("\nNo accessible name or automationId — this element can't be targeted by invoke/set_value/click_control; fall back to win_controls for a coordinate, or click(x,y) as a last resort.")
		}
		return b.String(), nil
	})
}

// ---- find by name / automationId ----

// findFirstByProp finds the first descendant whose property `propId` equals
// `value` (a string). Caller must release the returned element.
func findFirstByProp(uia *ole.IUnknown, root *ole.IUnknown, propId int, value string) (*ole.IUnknown, error) {
	bstr := ole.SysAllocString(value)
	if bstr == nil {
		return nil, fmt.Errorf("SysAllocString failed")
	}
	defer ole.SysFreeString(bstr)

	// VARIANT for a BSTR: vt = VT_BSTR (8), value = pointer to BSTR data.
	var v ole.VARIANT
	v.VT = 8 // VT_BSTR
	// Store the BSTR pointer into the VARIANT's value slot.
	*(*uintptr)(unsafe.Pointer(&v.Val)) = uintptr(unsafe.Pointer(bstr))

	var cond *ole.IUnknown
	hr := vcall(uia, uiaCreatePropertyCondition,
		uintptr(propId),
		// VARIANT is passed by value (struct copy) across the ABI; on amd64 a
		// 16-byte VARIANT is passed by reference to a hidden copy. go-ole's
		// VARIANT is 16 bytes (VT + reserved + Val), matching the Win32 layout
		// for the BSTR case, so we pass its address.
		uintptr(unsafe.Pointer(&v)),
		uintptr(unsafe.Pointer(&cond)))
	if failed(hr) || cond == nil {
		return nil, fmt.Errorf("CreatePropertyCondition failed (hr=0x%x)", uint32(hr))
	}
	defer release(cond)

	var found *ole.IUnknown
	hr = vcall(root, elemFindFirst, uintptr(treeScopeSubtree),
		uintptr(unsafe.Pointer(cond)), uintptr(unsafe.Pointer(&found)))
	if failed(hr) {
		return nil, fmt.Errorf("FindFirst failed (hr=0x%x)", uint32(hr))
	}
	return found, nil // may be nil if not found
}

// findByName tries Name first, then AutomationId.
func findByName(uia *ole.IUnknown, root *ole.IUnknown, name string) (*ole.IUnknown, error) {
	el, err := findFirstByProp(uia, root, uiaNamePropertyId, name)
	if err == nil && el != nil {
		return el, nil
	}
	if el != nil {
		release(el)
	}
	return findFirstByProp(uia, root, uiaAutomationIdPropertyId, name)
}

// findAllByProp finds every descendant whose property `propId` equals `value`
// and returns the resulting IUIAutomationElementArray plus its length. Caller
// must release the returned array (if non-nil).
func findAllByProp(uia *ole.IUnknown, root *ole.IUnknown, propId int, value string) (*ole.IUnknown, int32, error) {
	bstr := ole.SysAllocString(value)
	if bstr == nil {
		return nil, 0, fmt.Errorf("SysAllocString failed")
	}
	defer ole.SysFreeString(bstr)

	var v ole.VARIANT
	v.VT = 8 // VT_BSTR
	*(*uintptr)(unsafe.Pointer(&v.Val)) = uintptr(unsafe.Pointer(bstr))

	var cond *ole.IUnknown
	hr := vcall(uia, uiaCreatePropertyCondition,
		uintptr(propId), uintptr(unsafe.Pointer(&v)), uintptr(unsafe.Pointer(&cond)))
	if failed(hr) || cond == nil {
		return nil, 0, fmt.Errorf("CreatePropertyCondition failed (hr=0x%x)", uint32(hr))
	}
	defer release(cond)

	var arr *ole.IUnknown
	hr = vcall(root, elemFindAll, uintptr(treeScopeSubtree),
		uintptr(unsafe.Pointer(cond)), uintptr(unsafe.Pointer(&arr)))
	if failed(hr) || arr == nil {
		return nil, 0, fmt.Errorf("FindAll failed (hr=0x%x)", uint32(hr))
	}
	var l int32
	vcall(arr, arrGetLength, uintptr(unsafe.Pointer(&l)))
	return arr, l, nil
}

// firstMatchWhere returns the first element among all descendants whose
// property `propId` equals `value` that satisfies `pred`, or nil if none do.
// Non-matching elements (and the array) are released; the caller owns the
// returned element and must release it.
func firstMatchWhere(uia *ole.IUnknown, root *ole.IUnknown, propId int, value string, pred func(*ole.IUnknown) bool) *ole.IUnknown {
	arr, length, err := findAllByProp(uia, root, propId, value)
	if err != nil || arr == nil {
		return nil
	}
	defer release(arr)
	for i := int32(0); i < length; i++ {
		var el *ole.IUnknown
		hr := vcall(arr, arrGetElement, uintptr(i), uintptr(unsafe.Pointer(&el)))
		if failed(hr) || el == nil {
			continue
		}
		if pred(el) {
			return el
		}
		release(el)
	}
	return nil
}

// findByNamePreferring is like findByName but, when several elements share the
// same Name/AutomationId, prefers one satisfying `pred` over the bare
// document-order first match. This matters because some apps (e.g. Outlook's
// compose window) give a field's static label the EXACT same caption as its
// associated edit control — "제목(U)" names both the "Subject:" label text and
// the subject edit box — so a plain FindFirst can land on the inert label and
// the caller (which wanted the control behind that label) fails with a
// confusing "unsupported pattern" error even though the field is genuinely
// editable/invokable. Falls back to findByName (preserving its error) if no
// same-named element satisfies pred.
func findByNamePreferring(uia *ole.IUnknown, root *ole.IUnknown, name string, pred func(*ole.IUnknown) bool) (*ole.IUnknown, error) {
	if pred != nil {
		if el := firstMatchWhere(uia, root, uiaNamePropertyId, name, pred); el != nil {
			return el, nil
		}
		if el := firstMatchWhere(uia, root, uiaAutomationIdPropertyId, name, pred); el != nil {
			return el, nil
		}
	}
	return findByName(uia, root, name)
}

// ---- invoke ----

// invokeCapable reports whether el supports any pattern uiaInvoke can act
// through — used to disambiguate same-named elements in favor of the one
// uiaInvoke can actually activate (see findByNamePreferring).
func invokeCapable(el *ole.IUnknown) bool {
	return elemSupportsPattern(el, uiaInvokePatternId) ||
		elemSupportsPattern(el, uiaSelectionItemPatternId) ||
		elemSupportsPattern(el, uiaTogglePatternId) ||
		elemSupportsPattern(el, uiaExpandCollapsePatternId)
}

// valueCapable reports whether el supports the Value pattern — used to
// disambiguate same-named elements in favor of the editable one (see
// findByNamePreferring).
func valueCapable(el *ole.IUnknown) bool {
	return elemSupportsPattern(el, uiaValuePatternId)
}

// textCapable reports whether el exposes the UIA Text pattern — how Documents,
// read-only text areas and editors expose their content (they carry no Value
// pattern). Lets us read "what does this say" by handle instead of a screenshot.
func textCapable(el *ole.IUnknown) bool {
	return elemSupportsPattern(el, uiaTextPatternId)
}

// elemValue reads el's Value-pattern current text (the editable content of
// edit/combo controls). Returns ("", false) when el has no Value pattern.
func elemValue(el *ole.IUnknown) (string, bool) {
	p := getPattern(el, uiaValuePatternId)
	if p == nil {
		return "", false
	}
	defer release(p)
	var bstr *uint16
	if hr := vcall(p, valuePatternCurrentValue, uintptr(unsafe.Pointer(&bstr))); failed(hr) {
		return "", false
	}
	if bstr == nil {
		return "", true
	}
	s := ole.BstrToString(bstr)
	ole.SysFreeString((*int16)(unsafe.Pointer(bstr)))
	return s, true
}

// elemText reads el's content via the Text pattern's document range, truncated
// server-side to maxLen chars (pass a bounded value — never unbounded, so a huge
// document can't return megabytes). Returns ("", false) when el has no Text
// pattern, so callers can fall back to the Value pattern.
func elemText(el *ole.IUnknown, maxLen int) (string, bool) {
	tp := getPattern(el, uiaTextPatternId)
	if tp == nil {
		return "", false
	}
	defer release(tp)
	var rng *ole.IUnknown
	if hr := vcall(tp, textPatternGetDocumentRange, uintptr(unsafe.Pointer(&rng))); failed(hr) || rng == nil {
		return "", false
	}
	defer release(rng)
	if maxLen <= 0 {
		maxLen = 1
	}
	var bstr *uint16
	if hr := vcall(rng, textRangeGetText, uintptr(int32(maxLen)), uintptr(unsafe.Pointer(&bstr))); failed(hr) {
		return "", false
	}
	if bstr == nil {
		return "", true
	}
	s := ole.BstrToString(bstr)
	ole.SysFreeString((*int16)(unsafe.Pointer(bstr)))
	return s, true
}

// uiaInvoke finds an element by Name (or AutomationId) and activates it via the
// most appropriate pattern (Invoke → SelectionItem → Toggle → ExpandCollapse).
func uiaInvoke(name string) error {
	if err := beginSyntheticInput(); err != nil {
		return err
	}
	_, err := uiaDo(func(uia *ole.IUnknown) (string, error) {
		root, err := foregroundElement(uia)
		if err != nil {
			return "", err
		}
		defer release(root)

		el, err := findByNamePreferring(uia, root, name, invokeCapable)
		if err != nil {
			return "", err
		}
		if el == nil {
			return "", fmt.Errorf("no element named %q (try snapshot)", name)
		}
		defer release(el)

		if p := getPattern(el, uiaInvokePatternId); p != nil {
			defer release(p)
			if hr := vcall(p, invokePatternInvoke); failed(hr) {
				return "", fmt.Errorf("Invoke failed (hr=0x%x)", uint32(hr))
			}
			return "", nil
		}
		if p := getPattern(el, uiaSelectionItemPatternId); p != nil {
			defer release(p)
			if hr := vcall(p, selectionItemSelect); failed(hr) {
				return "", fmt.Errorf("Select failed (hr=0x%x)", uint32(hr))
			}
			return "", nil
		}
		if p := getPattern(el, uiaTogglePatternId); p != nil {
			defer release(p)
			if hr := vcall(p, togglePatternToggle); failed(hr) {
				return "", fmt.Errorf("Toggle failed (hr=0x%x)", uint32(hr))
			}
			return "", nil
		}
		if p := getPattern(el, uiaExpandCollapsePatternId); p != nil {
			defer release(p)
			if hr := vcall(p, expandCollapseExpand); failed(hr) {
				return "", fmt.Errorf("Expand failed (hr=0x%x)", uint32(hr))
			}
			return "", nil
		}
		return "", fmt.Errorf("element %q supports no invokable pattern", name)
	})
	return err
}

// ---- set_value ----

// uiaSetValue finds an element by Name (or AutomationId) and sets its text via
// the Value pattern.
func uiaSetValue(name, text string) error {
	if err := beginSyntheticInput(); err != nil {
		return err
	}
	_, err := uiaDo(func(uia *ole.IUnknown) (string, error) {
		root, err := foregroundElement(uia)
		if err != nil {
			return "", err
		}
		defer release(root)

		el, err := findByNamePreferring(uia, root, name, valueCapable)
		if err != nil {
			return "", err
		}
		if el == nil {
			return "", fmt.Errorf("no element named %q (try snapshot)", name)
		}
		defer release(el)

		p := getPattern(el, uiaValuePatternId)
		if p == nil {
			return "", fmt.Errorf("element %q does not support the Value pattern", name)
		}
		defer release(p)

		bstr := ole.SysAllocString(text)
		if bstr == nil {
			return "", fmt.Errorf("SysAllocString failed")
		}
		defer ole.SysFreeString(bstr)

		if hr := vcall(p, valuePatternSetValue, uintptr(unsafe.Pointer(bstr))); failed(hr) {
			return "", fmt.Errorf("SetValue failed (hr=0x%x)", uint32(hr))
		}
		return "", nil
	})
	return err
}

// ---- get_value ----

// uiaGetValue finds an element by Name (or AutomationId) and reads its
// current text via the UIA Value pattern — the read-side counterpart to
// uiaSetValue. Useful after set_value/type/invoke to confirm what a field
// actually holds now (e.g. autocomplete rewrote it, a calculated total field
// updated, or to check the current value before deciding what to set it to).
// A pure read, not synthetic input, so unlike uiaSetValue this does not go
// through beginSyntheticInput.
func uiaGetValue(name string) (string, error) {
	return uiaDo(func(uia *ole.IUnknown) (string, error) {
		root, err := foregroundElement(uia)
		if err != nil {
			return "", err
		}
		defer release(root)

		// Prefer a same-named element that actually carries readable text (a label
		// and its edit box often share a caption; we want the one with content).
		el, err := findByNamePreferring(uia, root, name, func(e *ole.IUnknown) bool {
			return valueCapable(e) || textCapable(e)
		})
		if err != nil {
			return "", err
		}
		if el == nil {
			return "", fmt.Errorf("no element named %q (try snapshot)", name)
		}
		defer release(el)

		if s, ok := elemValue(el); ok {
			return clampFieldValue(s), nil
		}
		// Documents / read-only text areas expose content via the Text pattern, not
		// Value — fall back so get_value reads them too instead of erroring out and
		// pushing the caller to a screenshot.
		if s, ok := elemText(el, maxFieldValueChars); ok {
			return clampFieldValue(s), nil
		}
		return "", fmt.Errorf("element %q supports neither the Value nor Text pattern (nothing to read; try get_text or snapshot)", name)
	})
}

// ---- get_text ----

// maxTextChars bounds get_text. Larger than a single field's value cap because
// reading a document/editor pane is the whole point — but still bounded so a huge
// document can't flood the prompt (the caller can re-read with a named sub-element
// or capture_region if it truly needs more).
const maxTextChars = 8000

// clampTextValue trims get_text output to maxTextChars runes with a marker, a
// belt-and-suspenders bound on top of the server-side GetText truncation.
func clampTextValue(s string) string {
	r := []rune(s)
	if len(r) <= maxTextChars {
		return s
	}
	return string(r[:maxTextChars]) + fmt.Sprintf("… [truncated, %d chars total]", len(r))
}

// uiaGetText reads a control's full textual content via the UIA Text pattern
// (with a Value-pattern fallback) so a worker can read documents, editors and
// read-only text panes BY HANDLE instead of screenshotting them. name is an
// element Name/AutomationId; an empty name targets the foreground window element
// itself (useful when the whole window is one text document). A pure read — no
// synthetic input.
func uiaGetText(name string) (string, error) {
	return uiaDo(func(uia *ole.IUnknown) (string, error) {
		root, err := foregroundElement(uia)
		if err != nil {
			return "", err
		}
		defer release(root)

		target := root
		if strings.TrimSpace(name) != "" {
			found, ferr := findByNamePreferring(uia, root, name, func(e *ole.IUnknown) bool {
				return textCapable(e) || valueCapable(e)
			})
			if ferr != nil {
				return "", ferr
			}
			if found == nil {
				return "", fmt.Errorf("no element named %q (try snapshot)", name)
			}
			defer release(found)
			target = found
		}

		if s, ok := elemText(target, maxTextChars); ok {
			return clampTextValue(s), nil
		}
		if s, ok := elemValue(target); ok {
			return clampTextValue(s), nil
		}
		if strings.TrimSpace(name) == "" {
			return "", fmt.Errorf("the foreground window exposes no Text pattern at its root; name a specific element from snapshot, or use capture_window as a last resort")
		}
		return "", fmt.Errorf("element %q supports neither the Text nor Value pattern", name)
	})
}

// maxFieldValueChars bounds a single UIA field value returned by get_value. The
// element COUNT is already capped (snapshot max=200), but a single control — a
// text editor, a document, a huge read-only box — could return its entire
// contents (tens of thousands of tokens) in one read. This is the missing
// per-element length cap; the value is trimmed with a marker so the caller knows
// it was cut.
const maxFieldValueChars = 2000

func clampFieldValue(s string) string {
	r := []rune(s)
	if len(r) <= maxFieldValueChars {
		return s
	}
	return string(r[:maxFieldValueChars]) + fmt.Sprintf("… [truncated, %d chars total]", len(r))
}

// ---- wait_for_control ----

// uiaWaitForControlPollEvery is how often uiaWaitForControl re-checks while
// waiting. A var (not const) so tests can shrink it.
var uiaWaitForControlPollEvery = 250 * time.Millisecond

// uiaControlExistsProbe checks whether an element named `name` currently
// exists in the foreground window's UIA tree. A package var (wrapping the
// real uiaDo/findByName-based check) so uiaWaitForControl's polling loop is
// unit-testable without a real UIA/COM worker thread — mirrors the
// findTopWindowProbe pattern already used for waitForWindow's tests.
var uiaControlExistsProbe = func(name string) (bool, error) {
	found, err := uiaDo(func(uia *ole.IUnknown) (string, error) {
		root, err := foregroundElement(uia)
		if err != nil {
			return "", err
		}
		defer release(root)
		el, err := findByName(uia, root, name)
		if err != nil {
			return "", err
		}
		if el == nil {
			return "", nil
		}
		defer release(el)
		return "found", nil
	})
	return err == nil && found == "found", err
}

// uiaWaitForControl polls the foreground window's UIA tree until an element
// matching name (Name or AutomationId) appears, or timeoutMs elapses —
// mirrors aglink-web's wait_for_element and aglink-screen's own
// wait_for_window, but for a specific control inside the foreground window
// rather than a whole top-level window. Replaces a manual snapshot-polling
// loop for dialogs/controls that render a moment after the action that
// triggers them (e.g. a "Save" dialog, a panel that appears after a click).
func uiaWaitForControl(name string, timeoutMs int) (string, error) {
	if timeoutMs <= 0 {
		timeoutMs = 8000
	}
	deadline := time.Now().Add(time.Duration(timeoutMs) * time.Millisecond)
	for {
		if ok, _ := uiaControlExistsProbe(name); ok {
			return fmt.Sprintf("ok: found %q", name), nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("timed out after %dms waiting for an element named %q (try snapshot)", timeoutMs, name)
		}
		time.Sleep(uiaWaitForControlPollEvery)
	}
}

// keep windows import referenced even if other helpers change.
var _ = windows.UTF16ToString
