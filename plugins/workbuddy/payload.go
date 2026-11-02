// payload.go rewrites the outgoing chat completion request body before it's
// forwarded upstream. The single-pass entry point is prepareUpstreamBody; the
// four *InPlace helpers are the field-level mutations it composes, and the
// legacy *ForUpstream / forceStreamBody wrappers exist for tests and other
// call sites that need them individually.
package main

import (
	"encoding/json"
	"regexp"
	"strings"
)

// toolDescriptionByteLimit is the threshold above which all tool descriptions
// are stripped to avoid Tencent's 64KB body-size filter. Mirrors OmniRoute.
const toolDescriptionByteLimit = 65536

// forceStreamBody returns the request body with "stream":true set, since the
// upstream rejects non-streaming chat requests.
// prepareUpstreamBody composes forceStreamBody + normalizeToolsForUpstream +
// rewriteSystemForUpstream + ensureSystemMessage + rewriteModelInBody into a
// single unmarshal/marshal pass (v0.6.31 perf: was 4-5 full JSON round-trips
// on every chat completion). The 4 legacy helpers remain for tests and other
// call sites that need them individually.
func prepareUpstreamBody(payload, original []byte, sa *storedAuth, upstreamModel string) []byte {
	src := payload
	if len(src) == 0 {
		src = original
	}
	if len(src) == 0 {
		return src
	}
	var obj map[string]any
	if json.Unmarshal(src, &obj) != nil {
		return src
	}

	// 1. forceStream: CodeBuddy rejects non-stream requests.
	obj["stream"] = true

	// 2. normalizeTools: tool_choice object form → string; "none" suppresses tools.
	normalizeToolsInPlace(obj)

	// 2.5 normalizeHistory: OpenAI developer 角色 → system（腾讯后端不认
	// developer，实测判 11128 渠道风控——Coding2API 2026-09 生产实证）；
	// 脏 tool_call（无 function/name）剔除，清空后无内容的 assistant 占位
	// 与悬空 role=tool 结果成对清理（大输入/长会话韧性，同源实证对策）。
	normalizeHistoryInPlace(obj)

	// 3. rewriteSystem: strip blocked Claude Code template phrases + force thinking.
	rewriteSystemInPlace(obj)

	// 3.2 sanitizeHistory: the same blocked phrases also arrive inside
	// assistant history echoes and user pastes (agent hosts quote their own
	// system prompt / config in conversation history). Tencent's 11-128
	// channel gate scans the WHOLE body, not just system messages —
	// 2026-10-21 replay evidence: a 50-message body with the phrases only in
	// msg[4] (user) and msg[9] (assistant, 15x) 400s even with a clean
	// system prompt; scrubbing every role to zero occurrences flips 400 →
	// 200. Word-level rewrites keep meaning intact, so the v0.9.18
	// system-only gate (which retired the wholesale neutralPrompt swap) is
	// not in scope here.
	sanitizeHistoryPhrasesInPlace(obj)

	// 3.25 sanitizeIdentity: the generalized self-introduction family
	// (verb table) and the DSH brand shrink, scoped to system/developer/
	// assistant only — the #574 boundary matrix shows the identity
	// sentences pass from a user message while 400ing from assistant
	// history, and rewriting user/tool content would desync the model's
	// view from what is on disk (#537). Runs AFTER 3.2 so the specific
	// phrase rewrites (which need the intact brand) win first, and BEFORE
	// the 3.5 reasoning fold so folded reasoning cannot re-arm the family.
	sanitizeIdentityInPlace(obj)

	// 3.3 typeUnionToolSchemas: a oneOf of objects with no type gets
	// type:object — kimi-k3.1-pro 400s "Invalid request parameters" on the
	// untyped union even when every branch is an object, and takes the same
	// body with the type beside it (magpie workbuddy 0.1.33 / #378).
	typeUnionToolSchemas(obj)

	// 3.5 injectReasoning: fold historical assistant reasoning_content into
	// content as <thought> blocks — the upstream drops the non-standard
	// field from multi-turn history and the model loses its own prior
	// chain of thought (issue #5).
	injectReasoningInPlace(obj)

	// 4. ensureSystemMessage: inject minimal system msg for Global only.
	ensureSystemMessageInPlace(obj, sa)

	// 5. rewriteModel: swap client model name to upstream model id.
	rewriteModelInPlace(obj, upstreamModel)

	// 5.5 clampReasoningEffort: the resolved upstream model rejects an
	// unsupported reasoning_effort (code 11150, live 2026-09-30) — clamp to
	// the nearest supported level. Runs AFTER forceMaxThinking (step 3), so
	// the hy3/hy4 pin semantics and the hy3/hy3-x low-keep stay intact.
	clampReasoningEffortInPlace(obj, upstreamModel)

	// 6. maxTokenRename: OpenAI-newer clients send max_completion_tokens;
	// the CodeBuddy upstream speaks max_tokens. Copy the value across when
	// max_tokens is absent, then drop the foreign key so the gateway does
	// not see an unknown parameter (wb2api payload.go, PR #116 semantics).
	if v, ok := obj["max_completion_tokens"]; ok {
		if _, has := obj["max_tokens"]; !has && v != nil {
			obj["max_tokens"] = v
		}
		delete(obj, "max_completion_tokens")
	}

	out, err := json.Marshal(obj)
	if err != nil {
		return src
	}
	return out
}

// normalizeHistoryInPlace normalizes message history before the upstream
// sees it (v0.9.15, large-input resilience):
//   - role "developer" → "system": the Tencent backend does not recognize
//     the OpenAI developer role and rejects the whole request as channel
//     risk-control 11128 (reasoning-model clients like PI convert system
//     messages to developer; Coding2API production evidence 2026-09).
//   - dirty tool_calls (missing function or empty name) are dropped; an
//     assistant placeholder left with no content after the cleanup is
//     dropped entirely; dangling role=tool results whose tool_call_id
//     points at a dropped call are dropped with it (same shape as the
//     Coding2API _clean_history_tool_calls defense). Oversized agent
//     histories trimmed by the client mid-conversation are the main
//     source of such orphans.
func normalizeHistoryInPlace(obj map[string]any) bool {
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return false
	}
	changed := false
	orphan := map[string]struct{}{}
	rewritten := make([]any, 0, len(msgs))
	for _, mi := range msgs {
		m, ok := mi.(map[string]any)
		if !ok {
			rewritten = append(rewritten, mi)
			continue
		}
		if role, _ := m["role"].(string); strings.EqualFold(role, "developer") {
			m["role"] = "system"
			changed = true
		}
		if tcs, ok := m["tool_calls"].([]any); ok {
			kept := make([]any, 0, len(tcs))
			for _, tci := range tcs {
				tc, ok := tci.(map[string]any)
				if !ok {
					continue
				}
				fn, _ := tc["function"].(map[string]any)
				name := ""
				if fn != nil {
					name, _ = fn["name"].(string)
				}
				if strings.TrimSpace(name) == "" {
					if id, _ := tc["id"].(string); id != "" {
						orphan[id] = struct{}{}
					}
					changed = true
					continue
				}
				kept = append(kept, tc)
			}
			if len(kept) == 0 {
				delete(m, "tool_calls")
				changed = true
				if cv, has := m["content"]; !has || cv == nil {
					continue // 清空后无内容的占位 assistant 整条丢弃
				}
			} else {
				m["tool_calls"] = kept
			}
		}
		rewritten = append(rewritten, m)
	}
	if len(orphan) > 0 {
		filtered := make([]any, 0, len(rewritten))
		for _, mi := range rewritten {
			m, ok := mi.(map[string]any)
			if !ok {
				filtered = append(filtered, mi)
				continue
			}
			if role, _ := m["role"].(string); strings.EqualFold(role, "tool") {
				id, _ := m["tool_call_id"].(string)
				if _, bad := orphan[id]; bad {
					changed = true
					continue
				}
			}
			filtered = append(filtered, m)
		}
		rewritten = filtered
	}
	if !changed {
		return false
	}
	obj["messages"] = rewritten
	return true
}

// normalizeToolsInPlace is the in-place form of normalizeToolsForUpstream.
// Returns true when obj was modified.
func normalizeToolsInPlace(obj map[string]any) bool {
	changed := false
	suppressTools := func() {
		if _, ok := obj["tools"]; ok {
			delete(obj, "tools")
			changed = true
		}
		if _, ok := obj["functions"]; ok {
			delete(obj, "functions")
			changed = true
		}
	}
	if tc, present := obj["tool_choice"]; present {
		switch v := tc.(type) {
		case string:
			if strings.EqualFold(strings.TrimSpace(v), "none") {
				delete(obj, "tool_choice")
				suppressTools()
				changed = true
			}
		case map[string]any:
			typ, _ := v["type"].(string)
			typ = strings.ToLower(strings.TrimSpace(typ))
			switch typ {
			case "none":
				delete(obj, "tool_choice")
				suppressTools()
				changed = true
			case "auto", "required":
				obj["tool_choice"] = typ
				changed = true
			case "function":
				name := ""
				if fn, ok := v["function"].(map[string]any); ok {
					name, _ = fn["name"].(string)
				}
				if name == "" {
					name, _ = v["name"].(string)
				}
				name = strings.TrimSpace(name)
				if name != "" {
					obj["tool_choice"] = name
				} else {
					obj["tool_choice"] = "auto"
				}
				changed = true
			default:
				delete(obj, "tool_choice")
				changed = true
			}
		default:
			delete(obj, "tool_choice")
			changed = true
		}
	}
	return changed
}

// rewriteSystemInPlace is the in-place form of rewriteSystemForUpstream.
// It does three things:
//  1. For each system message: apply sanitizeBlockedTemplates (blocked
//     phrases → safe variants). v0.9.28: the wholesale neutralPrompt swap
//     (length/agentPattern triggers) is retired — see
//     rewriteSystemContentField for the evidence chain.
//  2. Strip reasoning_effort "none"/"off" (Tencent rejects them); mirror
//     other values to reasoning_summary="auto" (OmniRoute codebuddy-cn.ts).
//  3. forceMaxThinking for hy3/hy4-family models.
func rewriteSystemInPlace(obj map[string]any) bool {
	changed := rewriteSystemMessagesInPlace(obj)
	if mirrorReasoningEffort(obj) {
		changed = true
	}
	if forceMaxThinking(obj) {
		changed = true
	}
	if compactToolDescriptions(obj) {
		changed = true
	}
	return changed
}

// rewriteSystemMessagesInPlace applies the content-filter defenses to SYSTEM
// messages only. v0.9.18 scope fix: the pre-0.9.18 loop fed EVERY message
// through the then-wholesale replacement, so any user paste / tool result /
// assistant history entry over the then-maxSystemPromptBytes (or matching the
// then-agentPattern) was silently rewritten to neutralPrompt and the upstream
// model saw a history full of hollow "You are a helpful AI assistant..."
// messages. Field reports mapping to exactly this: tool output that "looks
// truncated / stdout empty", conversations that "reset every so often", and
// the model answering
// with the neutral prompt itself on interruption. OmniRoute codebuddy-cn.ts
// (the porting source) gates the replacement on
// `message.role !== "system" -> return message` verbatim; this restores it.
func rewriteSystemMessagesInPlace(obj map[string]any) bool {
	messages, _ := obj["messages"].([]any)
	changed := false
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := msg["role"].(string); !strings.EqualFold(role, "system") {
			continue
		}
		if rewriteSystemContentField(msg) {
			changed = true
		}
	}
	return changed
}

// rewriteSystemContentField applies the filter defenses to one SYSTEM
// message's content. v0.9.28 (issue #3 closure, adopting PR #4's position):
// the wholesale neutralPrompt replacement — for content over
// maxSystemPromptBytes or matching agentPattern — is RETIRED. Evidence that
// it defended nothing: (a) the upstream filter blocklists VERBATIM phrases
// (sanitizeBlockedTemplates' one-word insert "tool" defeats it — a verbatim
// matcher does not reject by length or by broad agent-identity patterns);
// (b) PR #4's author removed both triggers and ran without 400s; (c) since
// the v0.9.18 role gate, non-system messages of any length or content pass
// unfiltered and no rejection was ever reported. What the wholesale swap DID
// do was gut every agent host's system prompt (nearly all match "you are
// claude code" / "you are a coding agent" and exceed 2000 bytes): tool-use
// rules, project context and behavior constraints were silently swapped for
// an 18-byte generic line — a permanent invisible degradation, strictly worse
// than a loud 400 that can be reported and fixed. System content now only
// goes through sanitizeBlockedTemplates (known blocked phrases → safe
// variants; everything else survives verbatim), on strings and per text part
// on arrays alike.
func rewriteSystemContentField(msg map[string]any) bool {
	switch c := msg["content"].(type) {
	case string:
		if r := sanitizeBlockedTemplates(c); r != c {
			msg["content"] = r
			return true
		}
	case []any:
		modified := false
		for _, p := range c {
			part, ok := p.(map[string]any)
			if !ok {
				continue
			}
			if t, ok := part["text"].(string); ok {
				if r := sanitizeBlockedTemplates(t); r != t {
					part["text"] = r
					modified = true
				}
			}
		}
		return modified
	}
	return false
}

// mirrorReasoningEffort implements OmniRoute codebuddy-cn.ts reasoning_effort
// handling:
//   - "none"/"off" → delete field (Tencent has no "none")
//   - other value → set reasoning_summary="auto" (mirror)
//   - absent → no-op (forcing reasoning triggers content filter)
func mirrorReasoningEffort(obj map[string]any) bool {
	eff, ok := obj["reasoning_effort"].(string)
	if !ok {
		return false
	}
	effLower := strings.ToLower(strings.TrimSpace(eff))
	if effLower == "none" || effLower == "off" {
		delete(obj, "reasoning_effort")
		return true
	}
	if effLower != "" {
		obj["reasoning_summary"] = "auto"
		return true
	}
	return false
}

// wbReasoningEffortRanks is the ascending rank of the reasoning-effort dials
// seen in the wild (OpenAI-style minimal/low/medium/high plus this roster's
// xhigh/max tiers). A model's supported set below is a subset of these keys;
// ranks exist only so an unsupported request can be clamped to the nearest
// supported level.
var wbReasoningEffortRanks = map[string]int{
	"minimal": 0,
	"low":     1,
	"medium":  2,
	"high":    3,
	"xhigh":   4,
	"max":     5,
}

// wbModelReasoningEfforts is the per-upstream-model supported effort set
// (2026-09-30 live upstream roster, from the 11150 rejections): a value
// outside the model's set is rejected upstream, so the payload path clamps
// instead of forwarding it.
var wbModelReasoningEfforts = map[string][]string{
	"deepseek-v4.1-flash": {"low", "high", "max"},
	"glm-5.3":             {"low", "high", "max"},
	"glm-5.3-flash":       {"low", "high", "max"},
	"kimi-k3-2":           {"low", "high", "xhigh"},
	"kimi-k2.8-preview":   {"low", "high", "max"},
	"hy4-preview":         {"high"},
	"hy3":                 {"low", "high"},
	"hy3-x":               {"low", "high"},
}

// clampReasoningEffortForModel returns the reasoning_effort value to send
// upstream for one resolved upstream model. Order of preference:
//
//  1. exact support (case-insensitive) → the normalized level, unchanged;
//  2. a request whose rank exceeds the model's top supported tier clamps to
//     that top tier (owner correction 2026-09-30: kimi-k3-2 "max" → "xhigh",
//     never a silent downgrade to "high");
//  3. nearest by rank (low<high<xhigh<max, with the wider minimal/medium
//     dialects placed between) — equidistant requests prefer the HIGHER
//     supported tier (never silently downgrade thinking);
//  4. low is the floor — a request below every supported level lands on the
//     lowest supported one.
//
// Unknown dials, empty values and unknown models pass through unchanged: the
// clamp never invents an effort level the client did not ask for (mirrors the
// "do not invent" boundary of mirrorReasoningEffort). Matching is
// case-insensitive because this runs on the client-facing body after model
// resolution.
func clampReasoningEffortForModel(model, effort string) string {
	set, ok := wbModelReasoningEfforts[strings.ToLower(strings.TrimSpace(model))]
	if !ok || len(set) == 0 {
		return effort
	}
	e := strings.ToLower(strings.TrimSpace(effort))
	for _, name := range set {
		if name == e {
			return e
		}
	}
	reqRank, ranked := wbReasoningEffortRanks[e]
	if !ranked {
		return effort // unknown dial: leave as-is, never invent
	}
	// Above-top clamp: a request ranked higher than every supported level
	// lands on the set's top tier (kimi-k3-2 low/high/xhigh + "max" → "xhigh").
	top, topRank := set[0], wbReasoningEffortRanks[set[0]]
	for _, name := range set[1:] {
		if rank := wbReasoningEffortRanks[name]; rank > topRank {
			top, topRank = name, rank
		}
	}
	if reqRank > topRank {
		return top
	}
	best := ""
	bestDist, bestRank := 0, 0
	for _, name := range set {
		rank := wbReasoningEffortRanks[name]
		dist := rank - reqRank
		if dist < 0 {
			dist = -dist
		}
		if best == "" || dist < bestDist || (dist == bestDist && rank > bestRank) {
			best, bestDist, bestRank = name, dist, rank
		}
	}
	return best
}

// clampReasoningEffortInPlace applies clampReasoningEffortForModel to
// obj["reasoning_effort"]. Absent/non-string fields are left alone; returns
// true when the field value changed.
func clampReasoningEffortInPlace(obj map[string]any, upstreamModel string) bool {
	cur, ok := obj["reasoning_effort"].(string)
	if !ok {
		return false
	}
	next := clampReasoningEffortForModel(upstreamModel, cur)
	if next == "" || next == cur {
		return false
	}
	obj["reasoning_effort"] = next
	return true
}

// typeUnionToolSchemaOneKeys / typeUnionToolSchemaListKeys / typeUnionToolSchemaMapKeys
// are the schema-keyword walk sets (magpie SUB_ONE / SUB_LIST / SUB_MAP,
// byte-aligned): only these keys are descended, so examples / default /
// const payloads are never touched.
var typeUnionToolSchemaOneKeys = []string{"items", "additionalItems", "unevaluatedItems", "contains", "not", "if", "then", "else", "propertyNames", "additionalProperties", "unevaluatedProperties"}
var typeUnionToolSchemaListKeys = []string{"anyOf", "oneOf", "allOf", "prefixItems"}
var typeUnionToolSchemaMapKeys = []string{"properties", "patternProperties", "$defs", "definitions", "dependentSchemas"}

// typeUnionToolSchemas walks obj["tools"] in all three shapes (the 0.9.72
// compaction discipline: nested function.parameters, flat Responses-API
// parameters, MCP-namespace sub-tools) and mends each parameters schema with
// schemaTypeUnions. Reports whether anything changed.
func typeUnionToolSchemas(obj map[string]any) bool {
	tools, ok := obj["tools"].([]any)
	if !ok || len(tools) == 0 {
		return false
	}
	changed := false
	var mendTool func(t any)
	mendTool = func(t any) {
		tool, ok := t.(map[string]any)
		if !ok {
			return
		}
		if fn, ok := tool["function"].(map[string]any); ok {
			if ps, ok := fn["parameters"].(map[string]any); ok {
				changed = schemaTypeUnions(ps, ps, 0) || changed
			}
		}
		if ps, ok := tool["parameters"].(map[string]any); ok {
			changed = schemaTypeUnions(ps, ps, 0) || changed
		}
		if subTools, ok := tool["tools"].([]any); ok {
			for _, sub := range subTools {
				mendTool(sub)
			}
		}
	}
	for _, t := range tools {
		mendTool(t)
	}
	return changed
}

// schemaTypeUnions gives a oneOf with no sibling type the type "object"
// when every branch is a typed object, itself or through a chain of local
// $refs (magpie workbuddy typeUnions, workbuddy 0.1.33 / issue #378:
// kimi-k3.1-pro answers 400 "Invalid request parameters" to such a union
// even when every branch is an object, and takes the same request with
// "type": "object" beside the oneOf — three real dsh runs with all 198
// tools). A union of other or mixed types, an anyOf, and a $ref that is
// missing, external or circular are left as they came. Only schema keywords
// are walked. Reports whether anything changed.
func schemaTypeUnions(s, root map[string]any, depth int) bool {
	if s == nil || depth > 64 {
		return false
	}
	changed := false
	if _, hasType := s["type"]; !hasType {
		if oneOf, ok := s["oneOf"].([]any); ok && len(oneOf) > 0 {
			allObject := true
			for _, b := range oneOf {
				if !typeUnionObjectBranch(root, b, nil) {
					allObject = false
					break
				}
			}
			if allObject {
				s["type"] = "object"
				changed = true
			}
		}
	}
	for _, k := range typeUnionToolSchemaOneKeys {
		if sub, ok := s[k].(map[string]any); ok {
			changed = schemaTypeUnions(sub, root, depth+1) || changed
		}
	}
	for _, k := range typeUnionToolSchemaListKeys {
		if list, ok := s[k].([]any); ok {
			for _, v := range list {
				if sub, ok := v.(map[string]any); ok {
					changed = schemaTypeUnions(sub, root, depth+1) || changed
				}
			}
		}
	}
	for _, k := range typeUnionToolSchemaMapKeys {
		m, ok := s[k].(map[string]any)
		if !ok {
			continue
		}
		for _, v := range m {
			if sub, ok := v.(map[string]any); ok {
				changed = schemaTypeUnions(sub, root, depth+1) || changed
			}
		}
	}
	return changed
}

// typeUnionObjectBranch says whether b is a typed object, itself or where its
// local $refs lead (magpie objectBranch): a $ref seen before on the way is a
// cycle, and not; more than 16 hops is not.
func typeUnionObjectBranch(root map[string]any, b any, seen []string) bool {
	branch, ok := b.(map[string]any)
	if !ok {
		return false
	}
	if t, has := branch["type"]; has {
		s, _ := t.(string)
		return s == "object"
	}
	ref, _ := branch["$ref"].(string)
	if ref == "" || len(seen) > 16 {
		return false
	}
	for _, s := range seen {
		if s == ref {
			return false
		}
	}
	target := typeUnionLocalRef(root, ref)
	if target == nil {
		return false
	}
	next := make([]string, len(seen), len(seen)+1)
	copy(next, seen)
	next = append(next, ref)
	return typeUnionObjectBranch(root, target, next)
}

// typeUnionLocalRef resolves a local "#/..." JSON pointer in root (with ~0 / ~1
// unescaping, magpie localRef); a non-local ref or a missing target returns nil.
func typeUnionLocalRef(root map[string]any, ref string) map[string]any {
	if !strings.HasPrefix(ref, "#/") {
		return nil
	}
	var at any = root
	for _, seg := range strings.Split(ref[2:], "/") {
		seg = strings.ReplaceAll(seg, "~1", "/")
		seg = strings.ReplaceAll(seg, "~0", "~")
		m, ok := at.(map[string]any)
		if !ok {
			return nil
		}
		next, ok := m[seg]
		if !ok {
			return nil
		}
		at = next
	}
	target, ok := at.(map[string]any)
	if !ok {
		return nil
	}
	return target
}

// compactToolDescriptions strips tool descriptions when the serialized tools
// array exceeds toolDescriptionByteLimit (64KB). Tencent's body-size filter
// rejects large tool descriptions. Mirrors OmniRoute codebuddy-cn.ts.
//
// v0.9.72 (issue #63): the old loop only understood the chat-completions
// nested shape (tool.function.description), so two Responses-API shapes slid
// through UNCOMPRESSED: flat function tools ({"type":"function","name":...,
// "description":...}) and MCP namespace tools ({"type":"namespace","name":
// "mcp__rea","description":...,"tools":[...]}). A Codex Desktop session with
// the full mcp__rea extension serializes 322KB of tool definitions — the
// compactor saw none of it (every tool hit the `continue`), the full payload
// reached Moonshot, and kimi-k3.1-pro answered upstream 400 code=11133 /
// extError 400001 "rejected by the model provider" 100% of the time. The
// cleaner now walks all three shapes recursively.
func compactToolDescriptions(obj map[string]any) bool {
	tools, ok := obj["tools"].([]any)
	if !ok || len(tools) == 0 {
		return false
	}
	serialized, err := json.Marshal(tools)
	if err != nil {
		return false
	}
	if len(serialized) < toolDescriptionByteLimit {
		return false
	}
	changed := false
	var cleanTool func(t any)
	cleanTool = func(t any) {
		tool, ok := t.(map[string]any)
		if !ok {
			return
		}
		// 1. Standard chat-completions nesting: tool.function.description.
		if fn, ok := tool["function"].(map[string]any); ok {
			if _, hasDesc := fn["description"]; hasDesc {
				delete(fn, "description")
				changed = true
			}
		}
		// 2. Flat shape (Responses API / built-in tools): description at the
		// tool's own top level. For the nested shape this key simply is not
		// present, so the delete is a no-op there.
		if _, hasDesc := tool["description"]; hasDesc {
			delete(tool, "description")
			changed = true
		}
		// 3. MCP namespace shape: the functions live under tool["tools"] —
		// the 170-tool mcp__rea payload carries almost all of its 322KB here.
		if subTools, ok := tool["tools"].([]any); ok {
			for _, sub := range subTools {
				cleanTool(sub)
			}
		}
	}
	for _, t := range tools {
		cleanTool(t)
	}
	return changed
}

// injectReasoningInPlace folds each historical assistant turn's reasoning
// back into its content as a <thought> block (issue #5, implementation
// adapted from PR #6). The upstream gateway silently drops the non-standard
// reasoning_content field on multi-turn history, so the model never sees its
// own prior chain of thought — "I derived X last turn" self-attention is
// gone and the model re-derives or contradicts itself. Inlining the reasoning
// into content survives the gateway's field whitelist. Idempotent: a message
// whose content already starts with <thought> is skipped.
func injectReasoningInPlace(obj map[string]any) bool {
	messages, ok := obj["messages"].([]any)
	if !ok || len(messages) == 0 {
		return false
	}
	changed := false
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		if !strings.EqualFold(strings.TrimSpace(role), "assistant") {
			continue
		}
		reasoning := reasoningTextFromMessage(msg)
		if reasoning == "" {
			continue
		}
		if prependThoughtInPlace(msg, "<thought>\n"+reasoning+"\n</thought>") {
			changed = true
		}
	}
	return changed
}

// reasoningTextFromMessage returns the first non-empty reasoning text carried
// on a message, from either reasoning_content or reasoning fields.
func reasoningTextFromMessage(msg map[string]any) string {
	for _, key := range []string{"reasoning_content", "reasoning"} {
		if rc, ok := msg[key].(string); ok && strings.TrimSpace(rc) != "" &&
			strings.TrimSpace(rc) != "[reasoning unavailable]" {
			return strings.TrimSpace(rc)
		}
	}
	return ""
}

// prependThoughtInPlace prepends thoughtBlock to a message's content when not
// already present. Handles plain string content and OpenAI-style content
// parts (the thought block becomes a new leading text part). Returns true
// when the message content was modified.
func prependThoughtInPlace(msg map[string]any, thoughtBlock string) bool {
	switch c := msg["content"].(type) {
	case string:
		if strings.HasPrefix(strings.TrimSpace(c), "<thought>") {
			return false
		}
		if strings.TrimSpace(c) == "" {
			msg["content"] = thoughtBlock
		} else {
			msg["content"] = thoughtBlock + "\n\n" + c
		}
		return true
	case []any:
		if len(c) == 0 {
			return false
		}
		// Multimodal (array-of-parts) content: prepend a text part
		// carrying the thought block before the first part.
		first, ok := c[0].(map[string]any)
		if !ok {
			return false
		}
		txt, ok := first["text"].(string)
		if !ok {
			return false
		}
		if strings.HasPrefix(strings.TrimSpace(txt), "<thought>") {
			return false
		}
		msg["content"] = append([]any{map[string]any{"type": "text", "text": thoughtBlock}}, c...)
		return true
	}
	return false
}

// ensureSystemMessageInPlace is the in-place form of ensureSystemMessage.
// Returns true when obj was modified.
//
// v0.9.37 (issue #20): the Global realm validates messages[0] — a payload
// whose FIRST message is not a system prompt is rejected (code 11128,
// "first message is not system prompt"; contract mirrored from
// zqcccc/workbuddy-cliproxy's ensureSystemFirst). The old any-position
// scan skipped the injection whenever a client carried a system message
// mid-history (normalizeHistory's developer→system rewrite preserves
// position), leaving a user first message for upstream to reject. A
// system message that is not first no longer suppresses the injection;
// the Global-only gate is unchanged so CN traffic stays byte-identical.
func ensureSystemMessageInPlace(obj map[string]any, sa *storedAuth) bool {
	if sa == nil || !(isGlobalDomain(sa.Auth.Domain) || isIntlDomain(sa.Auth.Domain)) {
		return false
	}
	messages, ok := obj["messages"].([]any)
	if !ok || len(messages) == 0 {
		return false
	}
	// Position, not presence (issue #20): only a system message AT THE
	// FRONT satisfies the upstream check.
	if first, ok := messages[0].(map[string]any); ok {
		if role, _ := first["role"].(string); strings.EqualFold(role, "system") {
			return false
		}
	}
	systemMsg := map[string]any{
		"role":    "system",
		"content": "You are a helpful assistant.",
	}
	obj["messages"] = append([]any{systemMsg}, messages...)
	return true
}

// rewriteModelInPlace swaps obj["model"] to upstreamModel when non-empty.
// Mirrors rewriteModelInBody's behavior (case-insensitive compare); returns
// true when modified.
func rewriteModelInPlace(obj map[string]any, upstreamModel string) bool {
	upstreamModel = strings.TrimSpace(upstreamModel)
	if upstreamModel == "" {
		return false
	}
	cur, _ := obj["model"].(string)
	if strings.EqualFold(strings.TrimSpace(cur), upstreamModel) {
		return false
	}
	obj["model"] = upstreamModel
	return true
}

func forceStreamBody(payload, original []byte) []byte {
	src := payload
	if len(src) == 0 {
		src = original
	}
	var obj map[string]any
	if json.Unmarshal(src, &obj) != nil {
		return src
	}
	obj["stream"] = true
	out, err := json.Marshal(obj)
	if err != nil {
		return src
	}
	return out
}

// normalizeToolsForUpstream adapts OpenAI tools / tool_choice fields to
// CodeBuddy's chat schema before the request is forwarded.
//
// Live-verified against /v2/chat/completions (2026-07):
//  1. tool_choice is typed as string on the upstream Go struct. OpenAI's object
//     form {"type":"function","function":{"name":"..."}} returns 400 code 11101
//     ("cannot unmarshal object into Go struct field Request.tool_choice of
//     type string"). Convert known object shapes to the matching string.
//  2. tool_choice "none" is accepted but ignored when tools[] is non-empty —
//     the model still emits tool_calls. The only reliable way to suppress tools
//     is to omit tools (and functions) entirely.
//
// String values auto / required / <function name> are left untouched.
func normalizeToolsForUpstream(payload []byte) []byte {
	if len(payload) == 0 {
		return payload
	}
	var obj map[string]any
	if json.Unmarshal(payload, &obj) != nil {
		return payload
	}
	changed := false

	suppressTools := func() {
		if _, ok := obj["tools"]; ok {
			delete(obj, "tools")
			changed = true
		}
		if _, ok := obj["functions"]; ok {
			delete(obj, "functions")
			changed = true
		}
	}

	if tc, present := obj["tool_choice"]; present {
		switch v := tc.(type) {
		case string:
			if strings.EqualFold(strings.TrimSpace(v), "none") {
				delete(obj, "tool_choice")
				suppressTools()
				changed = true
			}
		case map[string]any:
			typ, _ := v["type"].(string)
			typ = strings.ToLower(strings.TrimSpace(typ))
			switch typ {
			case "none":
				delete(obj, "tool_choice")
				suppressTools()
				changed = true
			case "auto", "required":
				obj["tool_choice"] = typ
				changed = true
			case "function":
				name := ""
				if fn, ok := v["function"].(map[string]any); ok {
					name, _ = fn["name"].(string)
				}
				if name == "" {
					name, _ = v["name"].(string)
				}
				name = strings.TrimSpace(name)
				if name != "" {
					obj["tool_choice"] = name
				} else {
					// Object force without a name: fall back to auto instead of 400.
					obj["tool_choice"] = "auto"
				}
				changed = true
			default:
				// Unknown object shape → drop rather than forward a 400.
				delete(obj, "tool_choice")
				changed = true
			}
		default:
			// null / array / number — drop to keep upstream happy.
			delete(obj, "tool_choice")
			changed = true
		}
	}

	if !changed {
		return payload
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return payload
	}
	return out
}

// rewriteSystemForUpstream neutralizes Claude Code template phrases that
// Tencent CodeBuddy's content filter blocklists verbatim — the agent identity
// line ("You are Claude Code, Anthropic's official CLI for Claude.") and the
// git injection ("Main branch (you will usually use this for PRs)"). Each
// rewrite is a single-word change so the prompt's meaning is preserved while
// dodging the exact-match filter.
func rewriteSystemForUpstream(payload []byte) []byte {
	if len(payload) == 0 {
		return payload
	}
	var obj map[string]any
	if json.Unmarshal(payload, &obj) != nil {
		return payload
	}
	changed := rewriteSystemMessagesInPlace(obj)
	if forceMaxThinking(obj) {
		changed = true
	}
	if !changed {
		return payload
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return payload
	}
	return out
}

// ensureSystemMessage injects a minimal system message unless the FIRST
// message is already a system prompt. Global (www.workbuddy.ai) rejects
// user-only requests with code 11101 "Parse message failed: 11101:invalid
// request", and per issue #20 field evidence any payload whose first
// message is not system with 11128 — the upstream check is on position,
// not on the presence of a system message somewhere in the list (mirrors
// zqcccc/workbuddy-cliproxy's ensureSystemFirst). CN (copilot.tencent.com)
// does not require a system message but tolerates one. Inserting a
// harmless system message unifies both paths.
func ensureSystemMessage(payload []byte, sa *storedAuth) []byte {
	if len(payload) == 0 {
		return payload
	}
	// Only inject for Global; CN doesn't need it and we minimize diff.
	if sa == nil || !(isGlobalDomain(sa.Auth.Domain) || isIntlDomain(sa.Auth.Domain)) {
		return payload
	}
	var obj map[string]any
	if json.Unmarshal(payload, &obj) != nil {
		return payload
	}
	messages, ok := obj["messages"].([]any)
	if !ok || len(messages) == 0 {
		return payload
	}
	// Position, not presence (issue #20): only a system message AT THE
	// FRONT satisfies the upstream check.
	if first, ok := messages[0].(map[string]any); ok {
		if role, _ := first["role"].(string); strings.EqualFold(role, "system") {
			return payload // first message already system
		}
	}
	systemMsg := map[string]any{
		"role":    "system",
		"content": "You are a helpful assistant.",
	}
	obj["messages"] = append([]any{systemMsg}, messages...)
	out, err := json.Marshal(obj)
	if err != nil {
		return payload
	}
	return out
}

// (rewriteContentField / sanitizeContentText were removed in v0.9.18: they
// applied the wholesale neutralPrompt replacement to messages of ANY role.
// Phrase-level rewrites are a different thing and are applied to every role
// since v0.9.63: the system-scoped ones live in rewriteSystemContentField
// above / rewriteSystemMessagesInPlace below, and the history-wide pass lives
// in sanitizeHistoryPhrasesInPlace. Neither ever replaces a whole message.)

// Blocked-template variants seen in the wild beyond the two verbatim
// templates above: capitalization drift, straight/curly quote drift, and
// "Anthropic's official CLI" phrasing without the "You are Claude Code"
// prefix (adapted from PR #4). The exact-match ReplaceAll calls above stay
// byte-identical to OmniRoute; these regexes are the safety net.
var (
	reClaudeCodeCli = regexp.MustCompile(`(?i)anthropic(?:'s)?\s+official\s+cli\s+for\s+claude`)
	reMainBranchPr  = regexp.MustCompile(`(?i)main\s+branch\s+\(you\s+will\s+usually\s+use\s+this\s+for\s+prs\)`)

	// reCodexCliIntro matches the Codex CLI base-instructions opening
	// signature that Tencent's gateway blocklists as channel risk-control
	// (issue #31, code 11128 "Illegal API invocation from an unapproved
	// channel"): the two-sentence intro every Codex CLI / Codex Desktop
	// session opens its system prompt with. Reporter's single-variable
	// replay: 16 tools / long context / reasoning all pass; only this
	// sentence pair 100% triggers 11128, and swapping it for a generic
	// line passes verbatim with the full 20k-char instruction intact.
	// Tolerant to case and to the first sentence ending with or without
	// further description before the period.
	reCodexCliIntro          = regexp.MustCompile(`(?i)you are a coding agent running in the codex cli[^.\n]*\.\s*codex cli is an open source project led by openai\s*\.?`)
	codexCliIntroReplacement = "You are a helpful coding assistant running in the terminal."

	// reDeepSeekHarnessIntro matches the DeepSeek Harness base-instructions
	// opening signature, the third member of the same 11-128 family as the
	// Claude Code and Codex CLI templates above. Single-variable replay
	// against copilot.tencent.com: the bare sentence "You are an AI agent
	// powered by DeepSeek Harness" returns 400 code 11-128 every time, while
	// "DeepSeek Harness" alone, "powered by DeepSeek Harness", "You are an
	// AI agent powered by DeepSeek." and the same sentence naming a different
	// harness all return 200. Swapping only this sentence in a 175 KB
	// request (47 messages, 66 tools, reasoning_effort=max) flips 400 -> 200.
	reDeepSeekHarnessIntro          = regexp.MustCompile(`(?i)you are an ai agent powered by deepseek harness`)
	deepSeekHarnessIntroReplacement = "You are an AI assistant"

	// reDeepSeekHarnessWebGUI matches the fourth member of the same 11-128
	// family as the intro signature above: the runtime-context line "the
	// DeepSeek Harness Web GUI" that DSH injects into every session. Single-
	// variable replay against copilot.tencent.com: the verbatim four-token
	// phrase "DeepSeek Harness Web GUI" returns 400 code 11-128, while
	// breaking any one token — "DeepSeek Harness Site GUI", "DeepSeek
	// Harness Web UI", "Harness Web GUI", "Web GUI" alone — returns 200.
	// Swap it for the verified-safe "web interface" phrasing.
	reDeepSeekHarnessWebGUI          = regexp.MustCompile(`(?i)deepseek\s+harness\s+web\s+gui`)
	deepSeekHarnessWebGUIReplacement = "DeepSeek Harness web interface"

	// v0.9.79 (field report 2026-10-27, model deepseek-v4.1-flash): the
	// 11-128 family was GENERALIZED server-side into a "client self-
	// introduction sentence pattern + brand" gate. workbuddy2api-hub
	// wb_proxy.py (#533/#537/#539, A/B-replayed against copilot.tencent.com)
	// verified three members and then generalized the verb table after
	// observing the upstream grow the rule one verb at a time:
	//
	//      You are an AI agent powered by DeepSeek Harness.
	//      You are a coding agent powered by the deepseek-v4.1-flash model.
	//      You are an AI assistant built on DeepSeek.
	//
	// The #574 boundary matrix (single-variable, CN realm only):
	// assistant/system carrying the sentence → 400/11128; user carrying the
	// same sentence → 200; inserting "the" before the brand → 200. So the
	// matcher below is ported from the reference with its bounded window —
	// up to three modifier words between article and verb, noun optional,
	// comma tolerated between noun and verb — which is what keeps sentences
	// like "You are free to use tools built by others" out of the match
	// (the verb sits four words deep there). RE2 has no lookahead, so the
	// sentence terminator is consumed and re-emitted by the replacer.
	// DIVERGENCE from the reference (deliberate, precision): the match must
	// start at a boundary — text start, newline, sentence punctuation +
	// space, or an opening quote/bracket (agents and models QUOTE prompts:
	// '"You are … powered by …"' in an assistant echo). Every confirmed
	// trigger sentence is boundary-initial in its prompt, and the boundary
	// keeps mid-sentence "you are" clauses ("The tools you are using were
	// built by Acme.") out of the rewrite.
	reIdentitySelfIntro = regexp.MustCompile(`(?i)(^|\n|[.!?;:,][ \t]+|["'(\[{][ \t]*)` +
		`you are\s+` +
		`(?:an?\s+|the\s+)?(?:[\w.-]+\s+){0,3}?` +
		`(?:agent|assistant|model|ai|llm)?[\s,]*` +
		`(?:powered\s+by|built\s+on|built\s+by|created\s+by|developed\s+by|made\s+by|trained\s+by)` +
		`[^\n]*?(?:\.(?:[ \t\r"]|\n|$)|\n|$)`)
	identitySelfIntroReplacement = "You are a helpful AI assistant."

	// reBrandProse shrinks the DSH brand in prose ("DeepSeek Harness" →
	// "DeepSeek") for the identity-scoped roles. #574's second finding: the
	// brand alone passes, but brand + the GUI description phrases ("this
	// page" / "this GUI" / "this app") co-occurring hits the same 11-128
	// check; shrinking just the brand passes while keeping the sentence
	// readable. Only space/tab-separated prose forms match — hyphenated or
	// underscored identifiers (/opt/deepseek-harness/app, deepseek_harness)
	// stay byte-identical (#574 measured that [ \t_-]* breaks them). Runs
	// AFTER the Web GUI four-token rewrite above (that rule needs the full
	// brand to match first).
	reBrandProse          = regexp.MustCompile(`(?i)deepseek[ \t]+harness`)
	brandProseReplacement = "DeepSeek"

	// reAnthropicBillingHeader strips the x-anthropic-billing-header line
	// Claude Code embeds in its first system block (magpie FLAGGED and
	// workbuddy2api-hub SANITIZE_HDR_RE both remove it; member of the same
	// 11-128 family). The bare-header rename below catches occurrences
	// whose value was formatted differently (no colon-value shape).
	reAnthropicBillingHeader = regexp.MustCompile(`(?i)x-anthropic-billing-header:[^;\r\n]*;?\s*`)
	reAnthropicBareHeader    = regexp.MustCompile(`(?i)x-anthropic-billing-header`)

	// reCodexSecondSentence matches the Codex CLI base-instructions SECOND
	// sentence on its own — the two-sentence reCodexCliIntro above needs
	// the "You are a coding agent…" opener adjacent, but Codex Desktop and
	// drifted releases send the attribution sentence alone (magpie FLAGGED
	// matches it with a lookbehind guard; RE2 has none, so the optional
	// "the " is absorbed and re-emitted, making the rewrite idempotent).
	reCodexSecondSentence          = regexp.MustCompile(`(?i)\b(?:the\s+)?codex\s+cli\s+is\s+an\s+open\s+source\s+project\s+led\s+by\s+openai\s*\.?`)
	codexSecondSentenceReplacement = "The Codex CLI is an open source project led by OpenAI."

	// reOMOJunior matches the OhMyOpenCode junior-agent fingerprint — a
	// confirmed contiguous 11-128 trigger (workbuddy2api-hub, A/B tested:
	// either token alone passes, only the full phrase 400s).
	reOMOJunior          = regexp.MustCompile(`(?i)sisyphus-junior\s*-\s*focused\s+executor\s+from\s+ohmyopencode`)
	omoJuniorReplacement = "Sisyphus-Junior - Focused executor"

	// v0.9.80: the pasted-error poisoning-loop breaker as a word-boundary
	// match. 0.9.79 used a bare substring replace, which also rewrote
	// the code embedded inside longer digit runs (probe: ts=1729111280
	// became ts=172911-1280, order 900111285 became 90011-1285) — user
	// data the model was asked to read, corrupted silently. The gate's
	// verified trigger shapes are the BARE code: "code 11128 ...",
	// `{"code":11128,...}` — the digits as their own token. \b (ASCII
	// word boundary) fires for every one of those (spaces, colons,
	// commas, quotes, CJK neighbors are all non-word bytes) and leaves
	// longer digit runs and letter-flanked forms untouched.
	rePastedErrorCode          = regexp.MustCompile(`\b11128\b`)
	pastedErrorCodeReplacement = "11-128"

	// v0.9.81: Claude Code embeds cc_* key-value metadata (cc_entrypoint=
	// and siblings) as text; the reference strips the pairs in every
	// role (wb_proxy.py SANITIZE_KV_RE, in tree since the #516 era) as
	// part of the same fingerprint family as the billing header above.
	// \b keeps identifier-shaped hosts (foo_cc_x, gcc_=...) out; the
	// fixpoint loop handles a pair whose value exposes another (the
	// reference loops until stable the same way).
	reCCKeyValue = regexp.MustCompile(`(?i)\bcc_[a-z0-9_]+=[^;\r\n]*;?\s*`)

	// v0.9.81: DSH's official @deepseek-ai/dsh-file-reference plugin
	// (pulled in via @deepseek-ai/dsh-api-session-controller) injects
	// FILE_REFERENCE_PROMPT into the system prompt, and that segment is
	// the FOURTH confirmed member of the 11-128 content family
	// (workbuddy2api-hub #675/#677/#680, fixes #681/#683/#684,
	// 2026-10-27/28 single-variable replays on the CN realm):
	//
	//   A: Tokens prefixed with @ are paths the user explicitly referenced.
	//   B: Relative paths resolve from the workspace root; absolute paths
	//      identify files or directories on the host.
	//   (+ a three-sentence tail about trailing slashes / read tool /
	//    @"..." quoting)
	//
	// Boundary replays: each sentence alone 200; A+B contiguous 400
	// (#683, 2026-10-27); the full five-sentence paragraph 400 while A+B
	// alone passed on the next day's pool (#684) — the gate tightened,
	// and the safe read is that the whole contiguous segment is the
	// match unit. Either way the minimal fix is the same and breaks
	// both: insert one "that" into sentence A, which preserves the
	// meaning verbatim and makes the segment non-contiguous. The
	// capture groups keep the original case (lowercase prompts stay
	// lowercase), and the rewrite is naturally idempotent — the fixed
	// sentence carries "paths that the user", which the pattern (paths
	// immediately followed by "the user") can never match again.
	// Scope matches the GUI segment: system/developer/assistant only.
	reDshFileRef = regexp.MustCompile(`(?i)(Tokens prefixed with @ are paths )(the user explicitly referenced\.)`)
)

func sanitizeBlockedTemplates(s string) string {
	s = strings.ReplaceAll(s,
		"You are Claude Code, Anthropic's official CLI for Claude.",
		"You are Claude Code, Anthropic's official CLI tool for Claude.")
	s = strings.ReplaceAll(s,
		"Main branch (you will usually use this for PRs)",
		"Default branch (you will usually use this for PRs)")
	s = reClaudeCodeCli.ReplaceAllString(s, "Anthropic's official CLI tool for Claude")
	s = reMainBranchPr.ReplaceAllString(s, "Default branch (you will usually use this for PRs)")
	// v0.9.54 (issue #31): Codex CLI intro signature — Tencent gateway
	// channel risk-control (11128). Exact match first (the verified
	// trigger), then the tolerant regex for case / phrasing drift between
	// Codex releases. Application scope is decided by the caller
	// (rewriteSystemMessagesInPlace for system/developer, and
	// sanitizeHistoryPhrasesInPlace for every other role since v0.9.63).
	s = strings.ReplaceAll(s,
		"You are a coding agent running in the Codex CLI, a terminal-based coding assistant. Codex CLI is an open source project led by OpenAI.",
		codexCliIntroReplacement)
	s = reCodexCliIntro.ReplaceAllString(s, codexCliIntroReplacement)
	// v0.9.62: DeepSeek Harness intro signature — same Tencent 11-128
	// channel gate. Same caller-decides scope as the Codex rule above.
	s = reDeepSeekHarnessIntro.ReplaceAllString(s, deepSeekHarnessIntroReplacement)
	// v0.9.62 follow-up: the DSH runtime-context line "the DeepSeek Harness
	// Web GUI" is a separate member of the same Tencent 11-128 phrase family
	// (verified by replay: only the full four-token phrase triggers).
	s = reDeepSeekHarnessWebGUI.ReplaceAllString(s, deepSeekHarnessWebGUIReplacement)
	// v0.9.79: Codex attribution sentence standalone (no opener adjacent).
	s = reCodexSecondSentence.ReplaceAllString(s, codexSecondSentenceReplacement)
	// v0.9.79: Claude Code's first system block embeds its billing header
	// as text; strip the value shape, then rename any bare leftovers.
	s = reAnthropicBillingHeader.ReplaceAllString(s, "")
	s = reAnthropicBareHeader.ReplaceAllString(s, "x-anthropic-billing-hdr")
	// v0.9.81: Claude Code's cc_* key-value metadata pairs, all roles —
	// same fingerprint family as the billing header (reference
	// SANITIZE_KV_RE). Every productive pass strictly shortens the
	// string, so the loop converges; the cap is pure paranoia.
	for i := 0; i < 8; i++ {
		r := reCCKeyValue.ReplaceAllString(s, "")
		if r == s {
			break
		}
		s = r
	}
	// v0.9.79: Claude Code feedback line — "give" phrasing is the verified
	// trigger spelling, "provide" passes (workbuddy2api-hub SANITIZE_REWRITES).
	s = strings.ReplaceAll(s,
		"To give feedback, users should report the issue at https://github.com/anthropics/claude-code/issues",
		"To provide feedback, users should report the issue at https://github.com/anthropics/claude-code/issues")
	// v0.9.79: OhMyOpenCode junior-agent fingerprint.
	s = reOMOJunior.ReplaceAllString(s, omoJuniorReplacement)
	// v0.9.79: the error code itself, pasted as text (users quote the 503
	// back into the agent while debugging), is a trigger and poisons every
	// later turn of that conversation — the hyphenated spelling passes and
	// reads the same. This is the poisoning-loop breaker; it runs in the
	// all-roles set precisely because pastes land in user messages.
	// v0.9.80: word-boundary match instead of a bare substring replace —
	// the gate trigger is the BARE code ("code 11128", `"code":11128`),
	// where the digits stand as their own token; a 11128 embedded inside a
	// longer digit run (timestamps like 1729111280, order numbers like
	// 900111285) is ordinary user data, and rewriting it silently corrupted
	// logs and code the model was asked to read (probe-verified on 0.9.79).
	// \b keeps every known paste shape covered — spaces, punctuation and
	// CJK neighbors are all non-word bytes, so the boundary fires — while
	// longer digit runs (and letter-flanked forms like x11128x) pass
	// through untouched, same as the reference's own trigger shape.
	s = rePastedErrorCode.ReplaceAllString(s, pastedErrorCodeReplacement)
	return s
}

// sanitizeHistoryPhrasesInPlace applies the blocked-phrase rewrites to EVERY
// message in the history — assistant echoes, tool results and user pastes
// included (v0.9.63). Evidence chain (2026-10-21, single-variable replays
// against copilot.tencent.com): a 50-message body whose only "DeepSeek
// Harness Web GUI" occurrences live in msg[4] (user, 1x) and msg[9]
// (assistant history echo of a DSH runtime-context summary, 15x) 400s with
// code 11128 even when the system prompt is fully sanitized; scrubbing the
// phrase in every role flips the same body to 200. The second trigger found
// the same way: the Claude Code git-injection line "Main branch (you will
// usually use this for PRs)" hits verbatim inside an assistant history echo
// (case-insensitive, every token load-bearing: MainX/branchX/PRx/
// broken-bracket variants all pass). The Tencent channel gate scans the
// whole request body, so the v0.9.18 system-only gate leaves these echoes
// exposed.
//
// Scope guard: this is NOT the retired v0.9.18 wholesale swap. Every
// rewrite is the same single-word/synonym substitution the system path has
// used since v0.9.28 — meaning-preserving, replay-verified safe — and
// content without a trigger phrase passes byte-identical.
//
// Role dispatch: none — every role goes through the same phrase set. Per-phrase
// role scoping is not trustworthy here: replay shows the Claude Code intro
// phrase passes untouched from an assistant message while the Web GUI phrase
// 400s from the same message, so which phrase the gate reads in which role is
// not modellable from outside. Phrase-level substitution is meaning-preserving
// in every role, so the blanket pass is the cheap side of that bet.
//
// Handles: string content; array content (per text part); tool_calls
// function.arguments (agents echo protocol text there). Assistant
// reasoning_content / reasoning mirrors are scrubbed HERE, before the
// injectReasoningInPlace fold (step 3.5) folds them into content — otherwise
// the fold would re-arm the phrases in already-sanitized content.
func sanitizeHistoryPhrasesInPlace(obj map[string]any) bool {
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return false
	}
	changed := false
	scrub := sanitizeBlockedTemplates
	for _, mi := range msgs {
		m, ok := mi.(map[string]any)
		if !ok {
			continue
		}
		// reasoning_content / reasoning (assistant-only fields) are folded
		// into content by injectReasoningInPlace AFTER this pass (step 3.5),
		// so sanitize them here or the fold re-arms the phrases.
		if role, _ := m["role"].(string); strings.EqualFold(strings.TrimSpace(role), "assistant") {
			for _, key := range []string{"reasoning_content", "reasoning"} {
				if rc, ok := m[key].(string); ok && rc != "" {
					if r := scrub(rc); r != rc {
						m[key] = r
						changed = true
					}
				}
			}
		}
		if c, ok := m["content"].(string); ok {
			if r := scrub(c); r != c {
				m["content"] = r
				changed = true
			}
		} else if parts, ok := m["content"].([]any); ok {
			for _, pi := range parts {
				p, ok := pi.(map[string]any)
				if !ok {
					continue
				}
				if t, ok := p["text"].(string); ok {
					if r := scrub(t); r != t {
						p["text"] = r
						changed = true
					}
				}
			}
		}
		if tcs, ok := m["tool_calls"].([]any); ok {
			for _, tci := range tcs {
				tc, ok := tci.(map[string]any)
				if !ok {
					continue
				}
				fn, ok := tc["function"].(map[string]any)
				if !ok || fn == nil {
					continue
				}
				if a, ok := fn["arguments"].(string); ok {
					if r := scrub(a); r != a {
						fn["arguments"] = r
						changed = true
					}
				}
			}
		}
	}
	return changed
}

// sanitizeIdentitySentences applies the generalized self-introduction
// rewrites to one text blob: the verb-family sentence first, then the DSH
// brand shrink. The brand shrink must run LAST — the Web GUI four-token
// rewrite in sanitizeBlockedTemplates (which ran earlier in the pipeline)
// needs the intact two-word brand to match its pattern.
func sanitizeIdentitySentences(s string) string {
	out := reIdentitySelfIntro.ReplaceAllStringFunc(s, func(m string) string {
		// The match carries a sentence-boundary prefix (^ | \n | .!?;: +
		// spaces) and may end on a consumed newline — RE2 has no lookahead,
		// so both are split off and re-emitted around the neutral sentence.
		// The prefix is pure ASCII boundary punctuation, and the body always
		// begins with "you are", so the first non-boundary byte is the split
		// point.
		p := 0
		for p < len(m) {
			switch m[p] {
			case '\n', '.', '!', '?', ';', ':', ',', ' ', '\t', '"', '\'', '(', '[', '{':
				p++
				continue
			}
			break
		}
		prefix := m[:p]
		if strings.HasSuffix(m, "\n") {
			return prefix + identitySelfIntroReplacement + "\n"
		}
		if n := len(m); n > 0 {
			// A period terminated by a space/tab/CR/quote was
			// consumed together with its follower — re-emit the
			// follower so the next word keeps its gap and a JSON
			// string value keeps its closing quote (identity
			// sentences land inside tool_calls arguments).
			switch m[n-1] {
			case ' ', '\t', '\r', '"':
				return prefix + identitySelfIntroReplacement + m[n-1:]
			}
		}
		return prefix + identitySelfIntroReplacement
	})
	// v0.9.81: DSH file-reference segment — runs between the verb family
	// and the brand shrink, mirroring the reference order (identity
	// sentence -> GUI segment -> file-reference -> brand).
	out = reDshFileRef.ReplaceAllString(out, "${1}that ${2}")
	out = reBrandProse.ReplaceAllString(out, brandProseReplacement)
	return out
}

// sanitizeIdentityInPlace applies sanitizeIdentitySentences to the
// identity-scoped roles ONLY: system, developer and assistant (v0.9.79).
//
// Scope evidence — workbuddy2api-hub #574 single-variable matrix on the CN
// realm: the identity sentence 400s (11-128) from system/developer/assistant
// and passes (200) from user; #537's rationale for not touching user/tool:
// those are user data, and rewriting them desyncs the model's view from what
// is on disk. This deliberately DIVERGES from sanitizeHistoryPhrasesInPlace
// (all roles): that pass carries phrases replay-verified to 400 from user
// messages too (the Web GUI four-token phrase), this one does not.
//
// Covered surfaces per message: content (string and array text parts),
// assistant reasoning_content/reasoning mirrors (scrubbed here so the step
// 3.5 fold cannot re-arm the family in already-sanitized content), and
// assistant tool_calls arguments (model-authored text where agents echo
// protocol and identity prose).
func sanitizeIdentityInPlace(obj map[string]any) bool {
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return false
	}
	changed := false
	for _, mi := range msgs {
		m, ok := mi.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		switch strings.ToLower(strings.TrimSpace(role)) {
		case "system", "developer", "assistant":
		default:
			continue
		}
		if rc, ok := m["reasoning_content"].(string); ok && rc != "" {
			if r := sanitizeIdentitySentences(rc); r != rc {
				m["reasoning_content"] = r
				changed = true
			}
		}
		if rc, ok := m["reasoning"].(string); ok && rc != "" {
			if r := sanitizeIdentitySentences(rc); r != rc {
				m["reasoning"] = r
				changed = true
			}
		}
		if c, ok := m["content"].(string); ok {
			if r := sanitizeIdentitySentences(c); r != c {
				m["content"] = r
				changed = true
			}
		} else if parts, ok := m["content"].([]any); ok {
			for _, pi := range parts {
				p, ok := pi.(map[string]any)
				if !ok {
					continue
				}
				if t, ok := p["text"].(string); ok {
					if r := sanitizeIdentitySentences(t); r != t {
						p["text"] = r
						changed = true
					}
				}
			}
		}
		if tcs, ok := m["tool_calls"].([]any); ok {
			for _, tci := range tcs {
				tc, ok := tci.(map[string]any)
				if !ok {
					continue
				}
				fn, ok := tc["function"].(map[string]any)
				if !ok || fn == nil {
					continue
				}
				if a, ok := fn["arguments"].(string); ok {
					if r := sanitizeIdentitySentences(a); r != a {
						fn["arguments"] = r
						changed = true
					}
				}
			}
		}
	}
	return changed
}

// forceMaxThinking pins reasoning_effort to "high" for hy3/hy4-family models
// (Tencent Hunyuan) so they always reason at maximum depth. CodeBuddy only
// honors "high" for deep thinking (medium/low/max/xhigh/ultra all fall back to
// no reasoning), so we override whatever the client sent. Matching is
// case-insensitive because this runs before rewriteModelInPlace swaps the
// client-facing model name for the upstream ID. Returns true if changed.
func forceMaxThinking(obj map[string]any) bool {
	model, _ := obj["model"].(string)
	lm := strings.ToLower(model)
	if !strings.HasPrefix(lm, "hy3") && !strings.HasPrefix(lm, "hy4") {
		return false
	}
	// v0.9.29 (fork libo0118/qoder-custom review): the current Hy3 catalog
	// explicitly advertises a "low" effort level, so an explicit low is kept
	// instead of being forced up — the historical high-pin only remains as the
	// fallback for values the model can't honor.
	if eff, _ := obj["reasoning_effort"].(string); eff == "low" && (lm == "hy3" || lm == "hy3-x") {
		return false
	}
	if eff, _ := obj["reasoning_effort"].(string); eff == "high" {
		return false
	}
	obj["reasoning_effort"] = "high"
	return true
}

// rewriteModelInBody replaces the "model" field of a chat-completions body
// with the resolved upstream model ID.
func rewriteModelInBody(body []byte, upstreamModel string) []byte {
	if len(body) == 0 || strings.TrimSpace(upstreamModel) == "" {
		return body
	}
	var obj map[string]any
	if json.Unmarshal(body, &obj) != nil {
		return body
	}
	cur, _ := obj["model"].(string)
	if strings.EqualFold(strings.TrimSpace(cur), strings.TrimSpace(upstreamModel)) {
		return body
	}
	obj["model"] = upstreamModel
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

func isEmptyValue(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case []any:
		return len(x) == 0
	case map[string]any:
		if len(x) == 0 {
			return true
		}
		// Legacy function_call shell: {"name":"","arguments":""} is the
		// upstream's terminal-chunk artifact, not a real call — treat as empty
		// when every value is itself empty.
		for _, val := range x {
			if !isEmptyValue(val) {
				return false
			}
		}
		return true
	}
	return false
}
