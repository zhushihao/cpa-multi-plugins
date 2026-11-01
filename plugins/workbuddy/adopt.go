// adopt.go migrates legacy codebuddy-cn auth files to the merged workbuddy
// provider. History: workbuddy and codebuddy-cn were separate plugins against
// the SAME backend (copilot.tencent.com) sharing one credit pool, so they are
// merged into this single plugin (v0.9.0). Files written by the old
// codebuddy-cn plugin carry type/provider "codebuddy-cn" and would otherwise
// be orphaned (the host routes auth files to plugins by the file's type
// field), so on startup we copy them into canonical workbuddy files:
//
//	codebuddy-cn-<uid>.json  ->  workbuddy-<uid>.json  (copy, source preserved)
//	type/provider: codebuddy-cn  ->  workbuddy
//	auth.loginPlatform = "ide"   (CodeBuddy IDE login origin -> X-IDE-* headers)
//
// The migration is idempotent, guarded by a min re-run interval, never blocks
// plugin registration (runs in a background goroutine), and — I6 policy — is
// NON-DESTRUCTIVE: the legacy file is only ever read. See adoptForeignAuths
// for the full policy note.
package main

import (
	"encoding/json"
	"log"
	"strings"
	"sync"
)

// startAdoptionOnce makes automatic adoption a ONE-TIME controlled migration
// per process (architect ruling 2026-09-29): repeated register/reconfigure
// calls must not rescan for legacy files in the background. A legacy file that
// appears after startup is only adopted after a process restart (or a
// deliberate re-import/login, which never goes through adoption).
var startAdoptionOnce sync.Once

// startAdoption kicks the one-time background migration.
func startAdoption() {
	startAdoptionOnce.Do(func() {
		go adoptForeignAuths()
	})
}

// isLegacyCodebuddyAuthName reports whether an auth file name originates from
// the removed codebuddy-cn plugin.
func isLegacyCodebuddyAuthName(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	return strings.HasPrefix(lower, "codebuddy-cn-") || lower == "codebuddy-cn.json"
}

// isLegacyCodebuddyIntlAuthName reports whether an auth file name originates
// from the removed codebuddy-intl plugin (merged in v0.11.0). Those accounts
// live on the codebuddy.ai realm and adopt into region-qualified
// workbuddy-intl-<uid>.json names (never collide with CN/Global files).
func isLegacyCodebuddyIntlAuthName(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	return strings.HasPrefix(lower, "codebuddy-intl-") || lower == "codebuddy-intl.json"
}

// adoptForeignAuths rewrites every legacy codebuddy-cn auth file into the
// canonical workbuddy form.
//
// I6-WORKBUDDY-IMPL NON-DESTRUCTIVE POLICY: this function NEVER deletes or
// rewrites the legacy file. The legacy auth directory can be shared with the
// still-running :3002 gateway, and the design's rollback depends on the old
// credentials staying intact (original adopt.go deleted the legacy file after
// adoption and deleted duplicates, which would strand :3002). Adoption is
// therefore "copy to canonical, preserve source":
//   - a UID that already has a canonical workbuddy file is left alone (no
//     re-save, no cleanup of the legacy copy);
//   - the legacy file is only read, never written or removed;
//   - token bytes are copied verbatim through parseStored/buildAuthFileJSON.
//
// Formally re-enabling cleanup requires a separate product decision plus a
// fresh destructive-path safety review (design: 凭据旧目录只读).
func adoptForeignAuths() {
	// Serialize with the plugin's own delete path (record tombstone → delete):
	// an adoption that decided to re-create a canonical file must not land
	// after a concurrent removal deleted it (no resurrection window).
	wbAdoptDeleteMu.Lock()
	defer wbAdoptDeleteMu.Unlock()

	// Fail closed: without a trustworthy tombstone store we cannot prove that a
	// legacy source was not deliberately removed. Legacy files stay untouched;
	// a deliberate re-import/login still works.
	if usable, reason := wbAdoptTombstonesUsable(); !usable {
		log.Printf("adopt: tombstone store unusable (%s) — automatic adoption skipped, legacy files left in place", reason)
		return
	}

	files, err := hostAuthListFn()
	if err != nil {
		log.Printf("adopt: host auth list failed: %v", err)
		return
	}
	// Index existing canonical workbuddy files by UID-bearing name.
	existing := make(map[string]struct{}, len(files))
	for _, f := range files {
		if !isLegacyCodebuddyAuthName(f.Name) && !isLegacyCodebuddyIntlAuthName(f.Name) {
			existing[strings.ToLower(f.Name)] = struct{}{}
		}
	}
	adopted, deduped, skipped := 0, 0, 0
	for _, f := range files {
		if !isLegacyCodebuddyAuthName(f.Name) && !isLegacyCodebuddyIntlAuthName(f.Name) {
			continue
		}
		phys, err := hostAuthGetPhysicalFn(f.AuthIndex)
		if err != nil {
			log.Printf("adopt %s: get failed: %v", f.Name, err)
			skipped++
			continue
		}
		sa, err := parseStored(phys.JSON)
		if err != nil || sa == nil || strings.TrimSpace(sa.Account.UID) == "" {
			log.Printf("adopt %s: unparsable or missing uid — left in place (re-import manually)", f.Name)
			skipped++
			continue
		}
		// Pin the Intl realm before deriving the canonical name: codebuddy.ai
		// accounts adopt into workbuddy-intl-<uid>.json and must carry the
		// domain + IDE login origin for routing/headers.
		if isLegacyCodebuddyIntlAuthName(f.Name) && strings.TrimSpace(sa.Auth.Domain) == "" {
			sa.Auth.Domain = "codebuddy.ai"
		}
		canonical := authFileNameFor(sa)
		if _, dup := existing[strings.ToLower(canonical)]; dup {
			// Same credential already present as a workbuddy auth. The legacy
			// copy is PRESERVED in place (non-destructive policy): :3002 may
			// still be reading it, and only a human decision may retire it.
			log.Printf("adopt %s: duplicate of %s — legacy file preserved in place", f.Name, canonical)
			deduped++
			continue
		}
		// N1 second audit: a canonical file that was deliberately deleted must
		// not be re-created from the preserved legacy source. The tombstone is
		// plugin-owned state; the legacy file itself stays untouched. A
		// re-import / re-login writes the canonical file directly and is never
		// blocked by this check.
		if wbIsAdoptTombstoned(sa) {
			log.Printf("adopt %s: %s was deliberately removed (tombstoned) — legacy source preserved, not re-adopted", f.Name, canonical)
			deduped++
			continue
		}
		// Pin the IDE login origin so requests carry the X-IDE-* client
		// headers the upstream saw when this token was minted.
		if strings.TrimSpace(sa.Auth.LoginPlatform) == "" {
			sa.Auth.LoginPlatform = "ide"
		}
		// Preserve an existing note if the file carried one.
		family := "codebuddy-cn"
		if isLegacyCodebuddyIntlAuthName(f.Name) {
			family = "codebuddy-intl"
		}
		note := "migrated from " + family
		var meta struct {
			Note string `json:"note"`
		}
		if json.Unmarshal(phys.JSON, &meta) == nil && strings.TrimSpace(meta.Note) != "" {
			note = meta.Note
		}
		raw, err := buildAuthFileJSON(sa, phys.Disabled, note, nil)
		if err != nil {
			log.Printf("adopt %s: encode failed: %v", f.Name, err)
			skipped++
			continue
		}
		if err := hostAuthPersistFn(canonical, "", raw); err != nil {
			log.Printf("adopt %s: save %s failed: %v", f.Name, canonical, err)
			skipped++
			continue
		}
		log.Printf("adopt %s: copied to %s (login platform: ide; legacy file preserved)", f.Name, canonical)
		adopted++
		existing[strings.ToLower(canonical)] = struct{}{}
	}
	if adopted+deduped+skipped > 0 {
		log.Printf("adopt: done — adopted %d, duplicates preserved %d, skipped %d", adopted, deduped, skipped)
	}
}
