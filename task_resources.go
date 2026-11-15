package main

import (
	"crypto/sha256"
	"fmt"
	"sort"
)

func resourceIdentity(r resourceRecord) string {
	if r.Token == "" {
		return ""
	}
	return fmt.Sprintf("%s:%x", r.Kind, sha256.Sum256([]byte(r.Token)))
}

func numberInventoryResources(inv *inventory) error {
	keys := []string{}
	for _, r := range inv.Resources {
		if r.Identity != "" {
			keys = append(keys, r.Identity)
		}
	}
	ids, err := assignTaskIDs(keys)
	if err != nil {
		return err
	}
	for i := range inv.Resources {
		inv.Resources[i].TaskID = ids[inv.Resources[i].Identity]
	}
	sort.SliceStable(inv.Resources, func(i, j int) bool { return inv.Resources[i].TaskID < inv.Resources[j].TaskID })
	return nil
}

func runResourceTaskID(id uint64, args []string) (int, bool, error) {
	records, err := collectLocalResources("")
	if err != nil {
		return 1, true, err
	}
	keys := []string{}
	for _, r := range records {
		if key := resourceIdentity(r); key != "" {
			keys = append(keys, key)
		}
	}
	ids, err := assignTaskIDs(keys)
	if err != nil {
		return 1, true, err
	}
	for _, r := range records {
		key := resourceIdentity(r)
		if ids[key] != id {
			continue
		}
		if r.Kind == "review" {
			switch args[0] {
			case "review":
				code, err := runReview([]string{r.Token})
				return code, true, err
			case "cancel":
				code, err := runCancel([]string{"review/" + r.Token})
				return code, true, err
			}
		} else if args[0] == "cancel" {
			return 0, true, stopResource(r.Kind, r.Name, key)
		}
		return 2, true, usage("%s is not supported for task %d (%s)", args[0], id, r.Kind)
	}
	return 0, false, nil
}
