// -*- Mode: Go; indent-tabs-mode: t -*-

/*
 * Copyright (C) 2016-2017 Canonical Ltd
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License version 3 as
 * published by the Free Software Foundation.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

package snapstate

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/snapcore/snapd/overlord/snapstate/backend"
	"github.com/snapcore/snapd/overlord/state"
	"github.com/snapcore/snapd/snap"
	"github.com/snapcore/snapd/strutil"
)

// AliasesStatus represents the global aliases status for a snap.
type AliasesStatus string

// Possible global aliases statuses for a snap.
const (
	EnabledAliases  AliasesStatus = "enabled"
	DisabledAliases AliasesStatus = "disabled"
)

// AliasTarget carries the targets of an alias in the context of snap.
// If Manual is set it is the target of an enabled manual alias.
// Auto is set to the target for an automatic alias, enabled or
// disabled depending on the aliases status of the whole snap.
type AliasTarget struct {
	Manual string `json:"manual,omitempty"`
	Auto   string `json:"auto,omitempty"`
}

// Effective returns the target to use based on the aliasesStatus of the whole snap, returns "" if the alias is disabled.
func (at *AliasTarget) Effective(aliasesStatus AliasesStatus) string {
	if at == nil {
		return ""
	}
	if at.Manual != "" {
		return at.Manual
	}
	if aliasesStatus == EnabledAliases {
		return at.Auto
	}
	return ""
}

/*
   State for aliases for a snap is tracked in SnapState with:

	type SnapState struct {
                ...
		Aliases       map[string]*AliasTarget `json:"aliases,omitempty"`
		AliasesStatus AliasesStatus           `json:"aliases-status,omitempty"`
	}
}

   There are two kinds of aliases:

   * automatic aliases listed with their target application in the
     snap-declaration of the snap (using AliasTarget.Auto)

   * manual aliases setup with "snap alias SNAP.APP ALIAS" (tracked
     using AliasTarget.Manual)

   Further

   * all automatic aliases of a snap are either enabled
     or disabled together (tracked with AliasesStatus)

   * disabling a manual alias removes it from disk and state (for
     simplicity there is no disabled state for manual aliases)

   * an AliasTarget with both Auto and Manual set is a manual alias
     that has the same name as an automatic one, the manual target
     is what wins

*/

// TODO: helper from snap
func composeTarget(snapName, targetApp string) string {
	if targetApp == snapName {
		return targetApp
	}
	return fmt.Sprintf("%s.%s", snapName, targetApp)
}

// applyAliasesChange applies the necessary changes to aliases on disk
// to go from prevAliases under the snap global prevStatus for
// automatic aliases to newAliases under newStatus for snapName.
// It assumes that conflicts have already been checked.
func applyAliasesChange(st *state.State, snapName string, prevStatus AliasesStatus, prevAliases map[string]*AliasTarget, newStatus AliasesStatus, newAliases map[string]*AliasTarget, be managerBackend) error {
	var add, remove []*backend.Alias
	for alias, prevTargets := range prevAliases {
		if _, ok := newAliases[alias]; ok {
			continue
		}
		// gone
		if effTgt := prevTargets.Effective(prevStatus); effTgt != "" {
			remove = append(remove, &backend.Alias{
				Name:   alias,
				Target: composeTarget(snapName, effTgt),
			})
		}
	}
	for alias, newTargets := range newAliases {
		prevTgt := prevAliases[alias].Effective(prevStatus)
		newTgt := newTargets.Effective(newStatus)
		if prevTgt == newTgt {
			// nothing to do
			continue
		}
		if prevTgt != "" {
			remove = append(remove, &backend.Alias{
				Name:   alias,
				Target: composeTarget(snapName, prevTgt),
			})
		}
		if newTgt != "" {
			add = append(add, &backend.Alias{
				Name:   alias,
				Target: composeTarget(snapName, newTgt),
			})
		}
	}
	err := be.UpdateAliases(add, remove)
	if err != nil {
		return err
	}
	return nil
}

// autoAliasesDeltaV2 compares the automatic aliases with the current snap
// declaration for the installed snaps with the given names (or all if
// names is empty) and returns changed and dropped auto-aliases by
// snap name.
// TODO: temporary name
func autoAliasesDeltaV2(st *state.State, names []string) (changed map[string][]string, dropped map[string][]string, err error) {
	var snapStates map[string]*SnapState
	if len(names) == 0 {
		var err error
		snapStates, err = All(st)
		if err != nil {
			return nil, nil, err
		}
	} else {
		snapStates = make(map[string]*SnapState, len(names))
		for _, name := range names {
			var snapst SnapState
			err := Get(st, name, &snapst)
			if err != nil {
				return nil, nil, err
			}
			snapStates[name] = &snapst
		}
	}
	var firstErr error
	changed = make(map[string][]string)
	dropped = make(map[string][]string)
	for snapName, snapst := range snapStates {
		aliases := snapst.Aliases
		info, err := snapst.CurrentInfo()
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		autoAliases, err := AutoAliases(st, info)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for alias, target := range autoAliases {
			curTarget := aliases[alias]
			if curTarget == nil || curTarget.Auto != target {
				changed[snapName] = append(changed[snapName], alias)
			}
		}
		for alias, target := range aliases {
			if target.Auto != "" && autoAliases[alias] == "" {
				dropped[snapName] = append(dropped[snapName], alias)
			}
		}
	}
	return changed, dropped, firstErr
}

// refreshAliases applies the current snap-declaration aliases
// considering which applications exist in info and produces new aliases
// for the snap.
func refreshAliases(st *state.State, info *snap.Info, curAliases map[string]*AliasTarget) (newAliases map[string]*AliasTarget, err error) {
	autoAliases, err := AutoAliases(st, info)
	if err != nil {
		return nil, err
	}

	newAliases = make(map[string]*AliasTarget, len(autoAliases))
	// apply the current auto-aliases
	for alias, target := range autoAliases {
		if info.Apps[target] == nil {
			// not an existing app
			continue
		}
		newAliases[alias] = &AliasTarget{Auto: target}
	}

	// carry over the current manual ones
	for alias, curTarget := range curAliases {
		if curTarget.Manual == "" {
			continue
		}
		if info.Apps[curTarget.Manual] == nil {
			// not an existing app
			continue
		}
		newTarget := newAliases[alias]
		if newTarget == nil {
			newAliases[alias] = &AliasTarget{Manual: curTarget.Manual}
		} else {
			// alias is both manually setup but has an underlying auto-alias
			newAliases[alias].Manual = curTarget.Manual
		}
	}
	return newAliases, nil
}

type AliasConflictError struct {
	Snap      string
	Alias     string
	Reason    string
	Conflicts map[string][]string
}

func (e *AliasConflictError) Error() string {
	if len(e.Conflicts) != 0 {
		errParts := []string{"cannot enable"}
		first := true
		for snapName, aliases := range e.Conflicts {
			if !first {
				errParts = append(errParts, "nor")
			}
			if len(aliases) == 1 {
				errParts = append(errParts, fmt.Sprintf("alias %q", aliases[0]))
			} else {
				errParts = append(errParts, fmt.Sprintf("aliases %s", strutil.Quoted(aliases)))
			}
			if first {
				errParts = append(errParts, fmt.Sprintf("for %q,", e.Snap))
				first = false
			}
			errParts = append(errParts, fmt.Sprintf("already enabled for %q", snapName))
		}
		// TODO: add recommendation about what to do next
		return strings.Join(errParts, " ")
	}
	return fmt.Sprintf("cannot enable alias %q for %q, %s", e.Alias, e.Snap, e.Reason)
}

func addAliasConflicts(st *state.State, skipSnap string, testAliases map[string]bool, aliasConflicts map[string][]string) error {
	snapStates, err := All(st)
	if err != nil {
		return err
	}
	for otherSnap, snapst := range snapStates {
		if otherSnap == skipSnap {
			// skip
			continue
		}
		status := snapst.AliasesStatus
		var confls []string
		if len(snapst.Aliases) < len(testAliases) {
			for alias, target := range snapst.Aliases {
				if testAliases[alias] && target.Effective(status) != "" {
					confls = append(confls, alias)
				}
			}
		} else {
			for alias := range testAliases {
				target := snapst.Aliases[alias]
				if target != nil && target.Effective(status) != "" {
					confls = append(confls, alias)
				}
			}
		}
		if len(confls) > 0 {
			aliasConflicts[otherSnap] = confls
		}
	}
	return nil
}

// checkAliasesStatConflicts checks candAliases under candStatus for
// conflicts against other snap aliases returning conflicting
// snaps and aliases for alias conflicts.
func checkAliasesConflicts(st *state.State, snapName string, candStatus AliasesStatus, candAliases map[string]*AliasTarget) (conflicts map[string][]string, err error) {
	var snapNames map[string]*json.RawMessage
	err = st.Get("snaps", &snapNames)
	if err != nil && err != state.ErrNoState {
		return nil, err
	}

	enabled := make(map[string]bool, len(candAliases))
	for alias, candTarget := range candAliases {
		if candTarget.Effective(candStatus) != "" {
			enabled[alias] = true
		} else {
			continue
		}
		namespace := alias
		if i := strings.IndexRune(alias, '.'); i != -1 {
			namespace = alias[:i]
		}
		// check against snap namespaces
		if snapNames[namespace] != nil {
			return nil, &AliasConflictError{
				Alias:  alias,
				Snap:   snapName,
				Reason: fmt.Sprintf("it conflicts with the command namespace of installed snap %q", namespace),
			}
		}
	}

	// check against enabled aliases
	conflicts = make(map[string][]string)
	if err := addAliasConflicts(st, snapName, enabled, conflicts); err != nil {
		return nil, err
	}
	if len(conflicts) != 0 {
		return conflicts, &AliasConflictError{Snap: snapName, Conflicts: conflicts}
	}
	return nil, nil
}
