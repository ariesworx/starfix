package board

// Key is one key press the board acts on.
type Key int

// The keys. Right opens like Enter, and Left goes back like Esc.
const (
	KeyUp Key = iota + 1
	KeyDown
	KeyPageUp
	KeyPageDown
	KeyHome
	KeyEnd
	KeyEnter
	KeyBack
	KeyRefresh
	KeyHelp
	KeyQuit
)

// Keys decodes raw terminal input into the keys the board knows, in
// order, skipping anything else: other letters, function keys, modified
// arrows, pasted text. A terminal writes each escape sequence whole, so
// an Esc at the end of the input is the Esc key, and a sequence cut off
// is dropped.
func Keys(b []byte) []Key {
	var out []Key
	for i := 0; i < len(b); i++ {
		if b[i] != 0x1b {
			if k, ok := byteKeys[b[i]]; ok {
				out = append(out, k)
			}
			continue
		}
		k, ok, end := escape(b, i)
		if ok {
			out = append(out, k)
		}
		i = end
	}
	return out
}

// escape decodes the input that starts with the Esc at b[i], returning
// its key, if the board knows it, and the index of its last byte.
func escape(b []byte, i int) (k Key, ok bool, end int) {
	if i+1 == len(b) {
		return KeyBack, true, i
	}
	switch b[i+1] {
	case 'O': // SS3: application-mode cursor keys
		if i+2 == len(b) {
			return 0, false, i + 1
		}
		k, ok := cursor[b[i+2]]
		return k, ok, i + 2
	case '[': // CSI: parameters, then a final byte in 0x40-0x7e
		j := i + 2
		for j < len(b) && (b[j] < 0x40 || b[j] > 0x7e) {
			j++
		}
		if j == len(b) {
			return 0, false, j - 1
		}
		k, ok := csi(b[i+2:j], b[j])
		return k, ok, j
	case 0x1b:
		// Alt and a sequence, ignored like Alt and any key; otherwise
		// Esc twice in one read, or Alt and Esc, which are the same
		// bytes: one Esc either way.
		if i+2 < len(b) && (b[i+2] == '[' || b[i+2] == 'O') {
			_, _, end := escape(b, i+1)
			return 0, false, end
		}
		return KeyBack, true, i + 1
	}
	return 0, false, i + 1 // Esc, then another key: Alt held, which the board ignores
}

// byteKeys maps single bytes to keys.
var byteKeys = map[byte]Key{
	'q': KeyQuit, 0x03: KeyQuit, // Ctrl-C, which raw mode delivers as a byte
	'j': KeyDown, 'k': KeyUp, 'g': KeyHome, 'G': KeyEnd,
	'\r': KeyEnter, '\n': KeyEnter, 0x7f: KeyBack, 0x08: KeyBack,
	'r': KeyRefresh, '?': KeyHelp,
}

// cursor maps the final byte of an unmodified cursor key to its key.
var cursor = map[byte]Key{'A': KeyUp, 'B': KeyDown, 'C': KeyEnter, 'D': KeyBack, 'H': KeyHome, 'F': KeyEnd}

// csi decodes a CSI sequence's parameters and final byte.
func csi(params []byte, final byte) (Key, bool) {
	if len(params) == 0 {
		k, ok := cursor[final]
		return k, ok
	}
	if final != '~' {
		return 0, false // a modified key, such as Ctrl-Up
	}
	switch string(params) {
	case "1", "7":
		return KeyHome, true
	case "4", "8":
		return KeyEnd, true
	case "5":
		return KeyPageUp, true
	case "6":
		return KeyPageDown, true
	}
	return 0, false
}
