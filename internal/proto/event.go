package proto

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Changed summarizes an event in a few words: the fields an update
// touched, the label, the edge, or the new issue's title.
func (e Event) Changed() string {
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
		names := make([]string, 0, len(keys))
		for k := range keys {
			names = append(names, k)
		}
		sort.Strings(names)
		return strings.Join(names, ", ")
	case "label.add", "label.remove":
		return fmt.Sprint(pick["label"])
	case "dep.add", "dep.remove":
		return fmt.Sprintf("%v %v", pick["type"], pick["to"])
	case "issue.create":
		return fmt.Sprintf("%q", pick["title"])
	}
	return ""
}
