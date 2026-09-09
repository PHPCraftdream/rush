package config

// Surgical JSON editing of an MCP config document: a hand-written scanner and member insert/remove that preserve the file's existing indentation, member order and trailing whitespace, because a config file is something a human edits too. Split out of store_mcp_transaction.go when the 1000-line file limit landed.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
)

type mcpRoot struct {
	raw map[string]json.RawMessage
	mcp map[string]json.RawMessage
}

func decodeMCPRoot(data []byte) (mcpRoot, error) {
	root := make(map[string]json.RawMessage)
	if len(data) > 0 {
		if err := json.Unmarshal(data, &root); err != nil {
			return mcpRoot{}, fmt.Errorf("failed to parse config file: %w", err)
		}
	}
	servers := make(map[string]json.RawMessage)
	if raw := root["mcp"]; len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &servers); err != nil {
			return mcpRoot{}, fmt.Errorf("failed to parse MCP config: %w", err)
		}
	}
	return mcpRoot{raw: root, mcp: servers}, nil
}

func literalMCPEntryExists(data []byte, name string) bool {
	root, err := decodeMCPRoot(data)
	if err != nil {
		return false
	}
	_, ok := root.mcp[name]
	return ok
}

func updateMCPFile(files *mcpLockedFiles, path, name string, mutate func(map[string]any) error) error {
	data, err := editMCPDocumentWithKind(files.mcpData(path), mcpDocumentRush, "update", name, name, MCPConfig{}, nil, mutate)
	if err != nil {
		return err
	}
	return files.setMCPData(path, data)
}

type mcpJSONMember struct {
	key                  string
	keyStart, keyEnd     int
	valueStart, valueEnd int
}

type mcpJSONObject struct {
	start, end int
	members    []mcpJSONMember
}

type mcpJSONEdit struct {
	start, end  int
	replacement []byte
}

type mcpDocumentKind uint8

const (
	mcpDocumentRush mcpDocumentKind = iota
	mcpDocumentExternal
)

// editMCPDocument changes only the selected MCP container or entry. The JSON
// decoder remains the semantic validator; the scanner supplies byte spans so
// unrelated user formatting never passes through a whole-document encoder.
func editMCPDocument(data []byte, path, operation, oldName, newName string, value MCPConfig, disabled *bool, mutate func(map[string]any) error) ([]byte, error) {
	return editMCPDocumentWithKind(data, mcpDocumentKindForPath(path), operation, oldName, newName, value, disabled, mutate)
}

func editMCPDocumentWithKind(data []byte, kind mcpDocumentKind, operation, oldName, newName string, value MCPConfig, disabled *bool, mutate func(map[string]any) error) ([]byte, error) {
	containerName := mcpContainerNameForKind(kind)
	var rootRaw map[string]json.RawMessage
	if len(data) > 0 {
		if err := json.Unmarshal(data, &rootRaw); err != nil {
			return nil, fmt.Errorf("failed to parse config file: %w", err)
		}
		if rootRaw == nil {
			rootRaw = make(map[string]json.RawMessage)
		}
	}

	if len(data) == 0 {
		return newMCPDocument(containerName, operation, oldName, newName, value, disabled, mutate)
	}
	root, err := scanMCPJSONObject(data)
	if err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}
	containerMember := lastMCPMember(root.members, containerName)
	var container *mcpJSONObject
	if containerMember != nil && string(data[containerMember.valueStart:containerMember.valueEnd]) != "null" {
		var entries map[string]json.RawMessage
		if err := json.Unmarshal(data[containerMember.valueStart:containerMember.valueEnd], &entries); err != nil {
			return nil, fmt.Errorf("failed to parse MCP config: %w", err)
		}
		if entries == nil {
			entries = make(map[string]json.RawMessage)
		}
		_, container, err = scanMCPJSONValue(data, containerMember.valueStart)
		if err != nil || container == nil {
			return nil, fmt.Errorf("failed to parse MCP config: expected %s to be an object", containerName)
		}
	}

	var entryRaw []byte
	if operation != "remove" {
		var entryErr error
		entryRaw, entryErr = mcpMutationEntry(data, container, oldName, newName, value, disabled, mutate)
		if entryErr != nil {
			return nil, entryErr
		}
	}
	entryName := newName
	if operation == "remove" {
		entryName = oldName
	}
	var entryMember *mcpJSONMember
	if container != nil {
		entryMember = lastMCPMember(container.members, entryName)
	}

	switch operation {
	case "remove":
		if container == nil {
			if containerMember != nil {
				return applyMCPJSONEdits(data, mcpJSONEdit{start: containerMember.valueStart, end: containerMember.valueEnd, replacement: []byte("{}")}), nil
			}
			return addMCPContainer(data, root, containerName, []byte("{}")), nil
		}
		if entryMember == nil {
			return data, nil
		}
		return removeMCPMembers(data, *container, oldName), nil
	case "replace":
		if oldName != newName && container != nil {
			if existing := lastMCPMember(container.members, newName); existing != nil {
				return nil, fmt.Errorf("MCP target already exists: %q", newName)
			}
			oldCount := 0
			for _, member := range container.members {
				if member.key == oldName {
					oldCount++
				}
			}
			if oldCount > 1 {
				withoutOld := removeMCPMembers(data, *container, oldName)
				withoutRoot, scanErr := scanMCPJSONObject(withoutOld)
				if scanErr != nil {
					return nil, scanErr
				}
				withoutMember := lastMCPMember(withoutRoot.members, containerName)
				if withoutMember == nil {
					return nil, errors.New("MCP container disappeared during rename")
				}
				_, withoutContainer, scanErr := scanMCPJSONValue(withoutOld, withoutMember.valueStart)
				if scanErr != nil || withoutContainer == nil {
					return nil, errors.New("MCP container became invalid during rename")
				}
				return insertMCPMember(withoutOld, *withoutContainer, newName, entryRaw), nil
			}
			if old := lastMCPMember(container.members, oldName); old != nil {
				return applyMCPJSONEdits(data,
					mcpJSONEdit{start: old.keyStart, end: old.keyEnd, replacement: mustJSONMarshal(newName)},
					mcpJSONEdit{start: old.valueStart, end: old.valueEnd, replacement: entryRaw}), nil
			}
		}
		fallthrough
	case "add", "disable", "update":
		if container == nil {
			containerValue := newMCPContainer(entryName, entryRaw, operation == "remove")
			if containerMember != nil {
				return applyMCPJSONEdits(data, mcpJSONEdit{start: containerMember.valueStart, end: containerMember.valueEnd, replacement: containerValue}), nil
			}
			return addMCPContainer(data, root, containerName, containerValue), nil
		}
		if entryMember != nil {
			return applyMCPJSONEdits(data, mcpJSONEdit{start: entryMember.valueStart, end: entryMember.valueEnd, replacement: entryRaw}), nil
		}
		return insertMCPMember(data, *container, entryName, entryRaw), nil
	default:
		return data, nil
	}
}

func mcpMutationEntry(data []byte, container *mcpJSONObject, oldName, newName string, value MCPConfig, disabled *bool, mutate func(map[string]any) error) ([]byte, error) {
	name := newName
	if disabled != nil || mutate != nil {
		name = oldName
	}
	var raw []byte
	if container != nil {
		if member := lastMCPMember(container.members, name); member != nil {
			raw = append([]byte(nil), data[member.valueStart:member.valueEnd]...)
		}
	}
	entry := make(map[string]any)
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &entry); err != nil {
			return nil, fmt.Errorf("failed to parse MCP server %q: %w", name, err)
		}
		if entry == nil {
			entry = make(map[string]any)
		}
	}
	if disabled != nil {
		entry["disabled"] = *disabled
	}
	if mutate != nil {
		if err := mutate(entry); err != nil {
			return nil, err
		}
	}
	if disabled == nil && mutate == nil {
		var err error
		raw, err = json.Marshal(value)
		return raw, err
	}
	return json.Marshal(entry)
}

func mcpContainerName(path string) string {
	return mcpContainerNameForKind(mcpDocumentKindForPath(path))
}

func mcpDocumentKindForPath(path string) mcpDocumentKind {
	if strings.EqualFold(filepath.Base(path), ".mcp.json") {
		return mcpDocumentExternal
	}
	return mcpDocumentRush
}

func mcpContainerNameForKind(kind mcpDocumentKind) string {
	if kind == mcpDocumentExternal {
		return "mcpServers"
	}
	return "mcp"
}

func newMCPDocument(containerName, operation, oldName, newName string, value MCPConfig, disabled *bool, mutate func(map[string]any) error) ([]byte, error) {
	if operation == "remove" {
		return []byte("{\n  " + string(mustJSONMarshal(containerName)) + ": {}\n}\n"), nil
	}
	entry, err := mcpMutationEntry(nil, nil, oldName, newName, value, disabled, mutate)
	if err != nil {
		return nil, err
	}
	return prettyMCPDocument(containerName, newName, entry), nil
}

func prettyMCPDocument(containerName, name string, entry []byte) []byte {
	var indented bytes.Buffer
	if err := json.Indent(&indented, entry, "", "  "); err != nil {
		indented.Write(entry)
	}
	lines := bytes.Split(indented.Bytes(), []byte{'\n'})
	var value bytes.Buffer
	value.Write(lines[0])
	for _, line := range lines[1:] {
		value.WriteByte('\n')
		value.WriteString("    ")
		value.Write(line)
	}
	return []byte("{\n  " + string(mustJSONMarshal(containerName)) + ": {\n    " + string(mustJSONMarshal(name)) + ": " + value.String() + "\n  }\n}\n")
}

func newMCPContainer(name string, entry []byte, empty bool) []byte {
	if empty {
		return []byte("{}")
	}
	key := mustJSONMarshal(name)
	return append(append(append([]byte{'{'}, key...), ':'), append(entry, '}')...)
}

func addMCPContainer(data []byte, root mcpJSONObject, name string, value []byte) []byte {
	return insertMCPMember(data, root, name, value)
}

func insertMCPMember(data []byte, object mcpJSONObject, name string, value []byte) []byte {
	key := mustJSONMarshal(name)
	member := append(append(append(append([]byte(nil), key...), ':'), value...), nil...)
	contentStart, contentEnd := object.start+1, object.end-1
	layout := mcpJSONLayoutForObject(data, object)
	if len(object.members) == 0 {
		if layout.multiline {
			replacement := []byte(layout.newline + layout.memberIndent + string(member) + layout.newline + layout.closingIndent)
			return applyMCPJSONEdits(data, mcpJSONEdit{start: contentStart, end: contentEnd, replacement: replacement})
		}
		return applyMCPJSONEdits(data, mcpJSONEdit{start: contentStart, end: contentEnd, replacement: member})
	}

	last := object.members[len(object.members)-1]
	trailing := data[last.valueEnd:contentEnd]
	if layout.multiline {
		replacement := append([]byte{','}, []byte(layout.newline+layout.memberIndent)...)
		replacement = append(replacement, member...)
		replacement = append(replacement, trailing...)
		return applyMCPJSONEdits(data, mcpJSONEdit{start: last.valueEnd, end: contentEnd, replacement: replacement})
	}
	separator := []byte(",")
	if bytes.Contains(data[contentStart:contentEnd], []byte(", ")) {
		separator = []byte(", ")
	}
	replacement := append(separator, member...)
	replacement = append(replacement, trailing...)
	return applyMCPJSONEdits(data, mcpJSONEdit{start: last.valueEnd, end: contentEnd, replacement: replacement})
}

func removeMCPMember(data []byte, object mcpJSONObject, member mcpJSONMember) []byte {
	index := -1
	for i := range object.members {
		if object.members[i].keyStart == member.keyStart {
			index = i
			break
		}
	}
	if len(object.members) == 1 {
		replacement := append([]byte(nil), data[member.valueEnd:object.end-1]...)
		return applyMCPJSONEdits(data, mcpJSONEdit{start: object.start + 1, end: object.end - 1, replacement: replacement})
	}
	if index < len(object.members)-1 {
		return applyMCPJSONEdits(data, mcpJSONEdit{start: member.keyStart, end: object.members[index+1].keyStart})
	}
	trailing := append([]byte(nil), data[member.valueEnd:object.end-1]...)
	return applyMCPJSONEdits(data, mcpJSONEdit{start: object.members[index-1].valueEnd, end: object.end - 1, replacement: trailing})
}

func removeMCPMembers(data []byte, object mcpJSONObject, key string) []byte {
	indices := make([]int, 0, 1)
	for index, member := range object.members {
		if member.key == key {
			indices = append(indices, index)
		}
	}
	if len(indices) <= 1 {
		if len(indices) == 0 {
			return data
		}
		return removeMCPMember(data, object, object.members[indices[0]])
	}
	if len(indices) == len(object.members) {
		last := object.members[len(object.members)-1]
		replacement := append([]byte(nil), data[last.valueEnd:object.end-1]...)
		return applyMCPJSONEdits(data, mcpJSONEdit{start: object.start + 1, end: object.end - 1, replacement: replacement})
	}

	edits := make([]mcpJSONEdit, 0, len(indices))
	for start := 0; start < len(indices); {
		runStart := indices[start]
		runEnd := runStart
		for start+1 < len(indices) && indices[start+1] == runEnd+1 {
			start++
			runEnd = indices[start]
		}
		if runEnd == len(object.members)-1 {
			previous := object.members[runStart-1]
			trailing := append([]byte(nil), data[object.members[runEnd].valueEnd:object.end-1]...)
			edits = append(edits, mcpJSONEdit{start: previous.valueEnd, end: object.end - 1, replacement: trailing})
		} else {
			edits = append(edits, mcpJSONEdit{
				start: object.members[runStart].keyStart,
				end:   object.members[runEnd+1].keyStart,
			})
		}
		start++
	}
	return applyMCPJSONEdits(data, edits...)
}

func lastMCPMember(members []mcpJSONMember, key string) *mcpJSONMember {
	for i := len(members) - 1; i >= 0; i-- {
		if members[i].key == key {
			member := members[i]
			return &member
		}
	}
	return nil
}

func applyMCPJSONEdits(data []byte, edits ...mcpJSONEdit) []byte {
	if len(edits) == 0 {
		return append([]byte(nil), data...)
	}
	ordered := slices.Clone(edits)
	slices.SortStableFunc(ordered, func(left, right mcpJSONEdit) int {
		if left.start < right.start {
			return -1
		}
		if left.start > right.start {
			return 1
		}
		if left.end < right.end {
			return -1
		}
		if left.end > right.end {
			return 1
		}
		return 0
	})

	maxInt := int(^uint(0) >> 1)
	resultLen := len(data)
	for index, edit := range ordered {
		if edit.start < 0 || edit.end < 0 || edit.start > edit.end || edit.end > len(data) {
			panic(fmt.Sprintf("invalid MCP JSON edit span %d:%d for document length %d", edit.start, edit.end, len(data)))
		}
		if index > 0 && ordered[index-1].end > edit.start {
			panic(fmt.Sprintf("overlapping MCP JSON edit spans %d:%d and %d:%d", ordered[index-1].start, ordered[index-1].end, edit.start, edit.end))
		}
		resultLen -= edit.end - edit.start
		if len(edit.replacement) > maxInt-resultLen {
			panic("MCP JSON edit result length overflows int")
		}
		resultLen += len(edit.replacement)
	}

	var result []byte
	if resultLen > 0 {
		result = make([]byte, resultLen)
	}
	sourceStart, destinationStart := 0, 0
	for _, edit := range ordered {
		destinationStart += copy(result[destinationStart:], data[sourceStart:edit.start])
		destinationStart += copy(result[destinationStart:], edit.replacement)
		sourceStart = edit.end
	}
	copy(result[destinationStart:], data[sourceStart:])
	return result
}

func mustJSONMarshal(value string) []byte {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return data
}

func mcpJSONNewline(data []byte) string {
	if bytes.Contains(data, []byte("\r\n")) {
		return "\r\n"
	}
	if bytes.Contains(data, []byte{'\n'}) {
		return "\n"
	}
	return ""
}

type mcpJSONLayout struct {
	newline       string
	multiline     bool
	memberIndent  string
	closingIndent string
}

func mcpJSONLayoutForObject(data []byte, object mcpJSONObject) mcpJSONLayout {
	contentStart, contentEnd := object.start+1, object.end-1
	if len(object.members) == 0 {
		gap := data[contentStart:contentEnd]
		newline := mcpJSONNewline(gap)
		if newline == "" {
			return mcpJSONLayout{}
		}
		closingIndent, ok := mcpJSONWhitespaceSuffixAfterNewline(gap)
		if !ok {
			return mcpJSONLayout{newline: newline, multiline: true}
		}
		return mcpJSONLayout{
			newline: newline, multiline: true,
			memberIndent: closingIndent + "  ", closingIndent: closingIndent,
		}
	}

	leading := data[contentStart:object.members[0].keyStart]
	trailing := data[object.members[len(object.members)-1].valueEnd:contentEnd]
	gaps := make([][]byte, 0, len(object.members)+1)
	gaps = append(gaps, leading)
	for index := 1; index < len(object.members); index++ {
		gaps = append(gaps, data[object.members[index-1].valueEnd:object.members[index].keyStart])
	}
	gaps = append(gaps, trailing)

	layout := mcpJSONLayout{}
	for index, gap := range gaps {
		newline := mcpJSONNewline(gap)
		if newline == "" {
			continue
		}
		layout.multiline = true
		if layout.newline == "" {
			layout.newline = newline
		}
		if index < len(gaps)-1 && layout.memberIndent == "" {
			if indent, ok := mcpJSONWhitespaceSuffixAfterNewline(gap); ok {
				layout.memberIndent = indent
			}
		}
	}
	if newline := mcpJSONNewline(trailing); newline != "" {
		if indent, ok := mcpJSONWhitespaceSuffixAfterNewline(trailing); ok {
			layout.closingIndent = indent
		}
	}
	return layout
}

func mcpJSONWhitespaceSuffixAfterNewline(data []byte) (string, bool) {
	index := bytes.LastIndexByte(data, '\n')
	if index < 0 {
		return "", false
	}
	suffix := data[index+1:]
	for _, char := range suffix {
		if char != ' ' && char != '\t' {
			return "", false
		}
	}
	return string(suffix), true
}

func mcpJSONMemberIndent(data []byte, keyStart int) string {
	lineStart := bytes.LastIndexByte(data[:keyStart], '\n') + 1
	indent := data[lineStart:keyStart]
	for _, char := range indent {
		if char != ' ' && char != '\t' {
			return ""
		}
	}
	return string(indent)
}

func scanMCPJSONObject(data []byte) (mcpJSONObject, error) {
	end, object, err := scanMCPJSONValue(data, mcpJSONSkipSpace(data, 0))
	if err != nil || object == nil || mcpJSONSkipSpace(data, end) != len(data) {
		if err != nil {
			return mcpJSONObject{}, err
		}
		return mcpJSONObject{}, errors.New("JSON root must be an object")
	}
	return *object, nil
}

func scanMCPJSONValue(data []byte, pos int) (int, *mcpJSONObject, error) {
	if pos >= len(data) {
		return 0, nil, errors.New("unexpected end of JSON")
	}
	switch data[pos] {
	case '{':
		object := &mcpJSONObject{start: pos}
		pos = mcpJSONSkipSpace(data, pos+1)
		if pos == len(data) {
			return 0, nil, errors.New("unterminated JSON object")
		}
		if data[pos] == '}' {
			object.end = pos + 1
			return object.end, object, nil
		}
		for {
			keyStart := mcpJSONSkipSpace(data, pos)
			keyEnd, key, err := scanMCPJSONString(data, keyStart)
			if err != nil {
				return 0, nil, err
			}
			pos = mcpJSONSkipSpace(data, keyEnd)
			if pos >= len(data) || data[pos] != ':' {
				return 0, nil, errors.New("JSON object member lacks colon")
			}
			valueStart := mcpJSONSkipSpace(data, pos+1)
			valueEnd, _, err := scanMCPJSONValue(data, valueStart)
			if err != nil {
				return 0, nil, err
			}
			object.members = append(object.members, mcpJSONMember{
				key: key, keyStart: keyStart, keyEnd: keyEnd,
				valueStart: valueStart, valueEnd: valueEnd,
			})
			pos = mcpJSONSkipSpace(data, valueEnd)
			if pos >= len(data) {
				return 0, nil, errors.New("unterminated JSON object")
			}
			if data[pos] == '}' {
				object.end = pos + 1
				return object.end, object, nil
			}
			if data[pos] != ',' {
				return 0, nil, errors.New("JSON object member lacks comma")
			}
			pos++
		}
	case '[':
		pos = mcpJSONSkipSpace(data, pos+1)
		if pos < len(data) && data[pos] == ']' {
			return pos + 1, nil, nil
		}
		for {
			var err error
			pos, _, err = scanMCPJSONValue(data, mcpJSONSkipSpace(data, pos))
			if err != nil {
				return 0, nil, err
			}
			pos = mcpJSONSkipSpace(data, pos)
			if pos >= len(data) {
				return 0, nil, errors.New("unterminated JSON array")
			}
			if data[pos] == ']' {
				return pos + 1, nil, nil
			}
			if data[pos] != ',' {
				return 0, nil, errors.New("JSON array member lacks comma")
			}
			pos++
		}
	case '"':
		end, _, err := scanMCPJSONString(data, pos)
		return end, nil, err
	default:
		end := pos
		for end < len(data) && !bytes.ContainsRune([]byte{' ', '\t', '\r', '\n', ',', ']', '}'}, rune(data[end])) {
			end++
		}
		if end == pos {
			return 0, nil, errors.New("invalid JSON value")
		}
		return end, nil, nil
	}
}

func scanMCPJSONString(data []byte, start int) (int, string, error) {
	if start >= len(data) || data[start] != '"' {
		return 0, "", errors.New("JSON object key must be a string")
	}
	for pos := start + 1; pos < len(data); pos++ {
		switch data[pos] {
		case '\\':
			pos++
		case '"':
			end := pos + 1
			var value string
			if err := json.Unmarshal(data[start:end], &value); err != nil {
				return 0, "", err
			}
			return end, value, nil
		}
	}
	return 0, "", errors.New("unterminated JSON string")
}

func mcpJSONSkipSpace(data []byte, pos int) int {
	for pos < len(data) {
		switch data[pos] {
		case ' ', '\t', '\r', '\n':
			pos++
		default:
			return pos
		}
	}
	return pos
}
