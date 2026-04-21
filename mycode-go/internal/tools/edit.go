package tools

import (
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
)

type editMeta struct {
	StartLine     int      `json:"start_line"`
	OldLineCount  int      `json:"old_line_count"`
	NewLineCount  int      `json:"new_line_count"`
	AddedLines    int      `json:"added_lines"`
	RemovedLines  int      `json:"removed_lines"`
	ContextBefore []string `json:"context_before"`
	ContextAfter  []string `json:"context_after"`
}

type editSpec struct {
	oldText string
	newText string
	prefix  string
	index   int
}

type editMatch struct {
	start   int
	end     int
	newText string
	index   int
}

// Edit applies one or more replacements against the original file content.
func (e *Executor) Edit(path string, edits []map[string]string) Result {
	if len(edits) == 0 {
		return errorResult("error: edits must not be empty")
	}

	filePath := ResolvePath(path, e.cwd)
	info, err := os.Stat(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return errorResult("error: file not found: " + path)
		}
		return errorResult(fmt.Sprintf("error: failed to read file: %v", err))
	}
	if info.IsDir() {
		return errorResult("error: not a file: " + path)
	}

	multi := len(edits) > 1
	items := make([]editSpec, 0, len(edits))
	for i, edit := range edits {
		oldText := edit["oldText"]
		newText := edit["newText"]
		prefix := ""
		if multi {
			prefix = fmt.Sprintf("edits[%d]: ", i)
		}
		if oldText == "" {
			return errorResult("error: " + prefix + "oldText must not be empty")
		}
		if oldText == newText {
			return errorResult("error: " + prefix + "oldText and newText are identical")
		}
		items = append(items, editSpec{oldText: oldText, newText: newText, prefix: prefix, index: i})
	}

	readMTime := info.ModTime().UnixNano()
	data, err := os.ReadFile(filePath)
	if err != nil {
		return errorResult(fmt.Sprintf("error: failed to read file: %v", err))
	}
	text := string(data)
	newline := ""
	if strings.Contains(text, "\r\n") {
		newline = "\r\n"
	}

	matches := make([]editMatch, 0, len(items))
	var normalizedText string
	var indexMap []int
	normalizedLoaded := false

	for _, edit := range items {
		exactCount := strings.Count(text, edit.oldText)
		if exactCount > 1 {
			return errorResult(fmt.Sprintf("error: %soldText occurs %d times; provide a more specific oldText", edit.prefix, exactCount))
		}
		if exactCount == 1 {
			start := strings.Index(text, edit.oldText)
			matches = append(matches, editMatch{
				start:   start,
				end:     start + len(edit.oldText),
				newText: edit.newText,
				index:   edit.index,
			})
			continue
		}

		if !normalizedLoaded {
			normalizedText, indexMap = normalizeText(text)
			normalizedLoaded = true
		}
		normalizedOld, _ := normalizeText(edit.oldText)
		normalizedCount := strings.Count(normalizedText, normalizedOld)
		if normalizedCount > 1 {
			return errorResult(fmt.Sprintf("error: %soldText occurs %d times after normalization; provide a more specific oldText", edit.prefix, normalizedCount))
		}
		if normalizedCount == 0 {
			modelText := "error: " + edit.prefix + "oldText not found"
			if hint := closestLineHint(text, edit.oldText); hint != "" {
				modelText += ". closest line: " + hint
			}
			return errorResult(modelText)
		}

		start := strings.Index(normalizedText, normalizedOld)
		end := start + len(normalizedOld)
		origStart := indexMap[start]
		origEnd := len(text)
		if end < len(indexMap) {
			origEnd = indexMap[end]
		}
		matches = append(matches, editMatch{
			start:   origStart,
			end:     origEnd,
			newText: edit.newText,
			index:   edit.index,
		})
	}

	sort.Slice(matches, func(i, j int) bool {
		return matches[i].start < matches[j].start
	})
	for i := 1; i < len(matches); i++ {
		if matches[i-1].end > matches[i].start {
			return errorResult(fmt.Sprintf("error: edits[%d] and edits[%d] overlap", matches[i-1].index, matches[i].index))
		}
	}

	updated := text
	for i := len(matches) - 1; i >= 0; i-- {
		match := matches[i]
		updated = updated[:match.start] + match.newText + updated[match.end:]
	}
	if updated == text {
		return errorResult("error: edits produced no changes")
	}

	infoAfterRead, err := os.Stat(filePath)
	if err == nil && infoAfterRead.ModTime().UnixNano() != readMTime {
		return errorResult("error: file changed while editing; read it again and retry")
	}
	if err := atomicWriteText(filePath, updated, newline); err != nil {
		return errorResult(fmt.Sprintf("error: failed to write file: %v", err))
	}

	metas := buildEditMetas(text, updated, matches)

	summary := "Updated " + path
	if len(items) > 1 {
		summary = fmt.Sprintf("Updated %s (%d edits)", path, len(items))
	}
	return Result{
		Output:   summary,
		Metadata: map[string]any{"edits": metas},
	}
}

func buildEditMetas(original, updated string, matches []editMatch) []editMeta {
	updatedLines := strings.Split(updated, "\n")
	metas := make([]editMeta, 0, len(matches))
	shift := 0
	for _, match := range matches {
		oldSnippet := original[match.start:match.end]
		newStart := match.start + shift
		startLine := strings.Count(updated[:newStart], "\n") + 1
		oldLineCount := max(1, splitLineCount(oldSnippet))
		newLineCount := max(1, splitLineCount(match.newText))
		lineIndex := startLine - 1
		addedLines, removedLines := lineEditStats(strings.Split(oldSnippet, "\n"), strings.Split(match.newText, "\n"))
		metas = append(metas, editMeta{
			StartLine:     startLine,
			OldLineCount:  oldLineCount,
			NewLineCount:  newLineCount,
			AddedLines:    addedLines,
			RemovedLines:  removedLines,
			ContextBefore: slices.Clone(updatedLines[max(0, lineIndex-3):lineIndex]),
			ContextAfter:  slices.Clone(updatedLines[min(len(updatedLines), lineIndex+newLineCount):min(len(updatedLines), lineIndex+newLineCount+3)]),
		})
		shift += len(match.newText) - (match.end - match.start)
	}
	return metas
}

func closestLineHint(text, needle string) string {
	needle = strings.TrimSpace(needle)
	if needle == "" {
		return ""
	}

	bestRatio := 0.0
	bestLine := ""
	for _, line := range strings.Split(text, "\n") {
		candidate := strings.TrimSpace(strings.TrimRight(line, "\r"))
		if candidate == "" {
			continue
		}
		ratio := similarityRatio(needle, candidate)
		if ratio > bestRatio {
			bestRatio = ratio
			bestLine = candidate
			if ratio >= 1 {
				break
			}
		}
	}
	if bestRatio < 0.6 || bestLine == "" {
		return ""
	}
	if len(bestLine) > 120 {
		return bestLine[:117] + "..."
	}
	return bestLine
}

func similarityRatio(a, b string) float64 {
	ar := []rune(a)
	br := []rune(b)
	if len(ar) == 0 && len(br) == 0 {
		return 1
	}
	if len(ar) == 0 || len(br) == 0 {
		return 0
	}

	prev := make([]int, len(br)+1)
	curr := make([]int, len(br)+1)
	for i := 1; i <= len(ar); i++ {
		curr[0] = i
		for j := 1; j <= len(br); j++ {
			cost := 0
			if ar[i-1] != br[j-1] {
				cost = 1
			}
			curr[j] = min(prev[j]+1, min(curr[j-1]+1, prev[j-1]+cost))
		}
		copy(prev, curr)
	}
	maxLen := max(len(ar), len(br))
	return 1 - float64(prev[len(br)])/float64(maxLen)
}

func lineEditStats(oldLines, newLines []string) (added int, removed int) {
	if len(oldLines) > 0 && oldLines[len(oldLines)-1] == "" {
		oldLines = oldLines[:len(oldLines)-1]
	}
	if len(newLines) > 0 && newLines[len(newLines)-1] == "" {
		newLines = newLines[:len(newLines)-1]
	}
	if len(oldLines) == 0 && len(newLines) == 0 {
		return 0, 0
	}

	lcs := make([][]int, len(oldLines)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(newLines)+1)
	}
	for i := len(oldLines) - 1; i >= 0; i-- {
		for j := len(newLines) - 1; j >= 0; j-- {
			if oldLines[i] == newLines[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
				continue
			}
			lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
		}
	}
	common := lcs[0][0]
	return len(newLines) - common, len(oldLines) - common
}

func normalizeText(text string) (string, []int) {
	var builder strings.Builder
	indexMap := make([]int, 0, len(text))
	pos := 0
	for _, line := range splitLinesKeepEnds(text) {
		content := strings.TrimRight(line, "\r\n")
		trimmed := strings.TrimRight(content, " \t")
		builder.WriteString(trimmed)
		for i := 0; i < len(trimmed); i++ {
			indexMap = append(indexMap, pos+i)
		}
		if len(content) != len(line) {
			builder.WriteByte('\n')
			indexMap = append(indexMap, pos+len(content))
		}
		pos += len(line)
	}
	return builder.String(), indexMap
}

func splitLinesKeepEnds(text string) []string {
	if text == "" {
		return nil
	}
	lines := []string{}
	start := 0
	for start < len(text) {
		end := start
		for end < len(text) && text[end] != '\n' && text[end] != '\r' {
			end++
		}
		if end == len(text) {
			lines = append(lines, text[start:end])
			break
		}
		if text[end] == '\r' && end+1 < len(text) && text[end+1] == '\n' {
			end += 2
		} else {
			end++
		}
		lines = append(lines, text[start:end])
		start = end
	}
	return lines
}
