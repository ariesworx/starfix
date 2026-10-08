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
// epoch and holder, or the new issue's title. It returns "" for any other
// op.
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
	case "claim.take", "claim.expire":
		if h, ok := pick["holder"].(map[string]any); ok {
			return fmt.Sprintf("epoch %v, %v/%v", pick["epoch"], h["principal"], h["session"])
		}
		return fmt.Sprintf("epoch %v", pick["epoch"])
	}
	return ""
}
