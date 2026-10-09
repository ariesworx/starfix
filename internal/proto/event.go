package proto

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// Changed summarizes an event in a few words: the fields an update, close
// or reopen touched, the label, the edge, the acceptance item, the claim's
// epoch and holder, the new issue's title, or whose hours on which day.
// It returns "" for any other op.
func (e Event) Changed() string {
	// The summary is best effort: a state that is absent or not an object
	// leaves its map nil, and it contributes nothing.
	var before, after map[string]any
	_ = json.Unmarshal(e.Before, &before)
	_ = json.Unmarshal(e.After, &after)
	pick := after
	if pick == nil {
		pick = before
	}
	switch e.Op {
	case "issue.update", "issue.close", "issue.reopen":
		keys := map[string]bool{}
		for k := range before {
			keys[k] = true
		}
		for k := range after {
			keys[k] = true
		}
		return strings.Join(slices.Sorted(maps.Keys(keys)), ", ")
	case "label.add", "label.remove":
		return fmt.Sprint(pick["label"])
	case "dep.add", "dep.remove":
		return fmt.Sprintf("%v %v", pick["type"], pick["to"])
	case "acceptance.tick", "acceptance.untick", "acceptance.waive":
		return fmt.Sprintf("item %v", pick["n"])
	case "issue.create":
		return fmt.Sprintf("%q", pick["title"])
	case "issue.paths":
		out := fmt.Sprint(pick["source"])
		for _, k := range []struct{ key, sign string }{{"added", "+"}, {"removed", "-"}} {
			n := 0
			if l, ok := pick[k.key].([]any); ok {
				n = len(l)
			}
			if c, ok := pick[k.key+"_count"].(float64); ok {
				n = int(c)
			}
			if n > 0 {
				out += fmt.Sprintf(" %s%d", k.sign, n)
			}
		}
		return out
	case "hours.log", "hours.delete":
		secs, _ := pick["seconds"].(float64)
		return fmt.Sprintf("%v %s on %v", pick["principal"], Hours(int64(secs)), pick["on"])
	case "claim.take", "claim.expire", "claim.release":
		if h, ok := pick["holder"].(map[string]any); ok {
			return fmt.Sprintf("epoch %v, %v/%v", pick["epoch"], h["principal"], h["session"])
		}
		return fmt.Sprintf("epoch %v", pick["epoch"])
	}
	return ""
}
