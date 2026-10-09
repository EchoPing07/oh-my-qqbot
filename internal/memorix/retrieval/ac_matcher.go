package retrieval

import (
	"sort"
	"strings"
)

// ACMatch 是一次 Aho-Corasick 命中。
type ACMatch struct {
	// Pattern 命中的模式（调用方传入的原始字符串）。
	Pattern string
	// Start 是命中在输入文本中的 rune 起始下标（含）。
	Start int
	// End 是命中在输入文本中的 rune 结束下标（不含）。
	End int
}

// AhoCorasick 是纯 Go 的 Aho-Corasick 自动机（goto 表 + fail 指针 + 输出集合），
// 语义对齐 Python 版 core/utils/matcher.py 的 AhoCorasick：
// 全局匹配（overlapping），find_all 返回出现次数。
//
// 出处：core/utils/matcher.py L28-L171。
type AhoCorasick struct {
	nextStates []map[rune]int
	fail       []int
	output     [][]string
	patternSet map[string]struct{}
	built      bool
	revision   int

	// 命中谓词：同一模式在重叠位置会被重复计数，与 Python 版一致。
	emitAll bool
}

// NewAhoCorasick 创建空匹配器。
func NewAhoCorasick() *AhoCorasick {
	return &AhoCorasick{
		nextStates: []map[rune]int{{}},
		fail:       []int{0},
		output:     [][]string{{}},
		patternSet: map[string]struct{}{},
		emitAll:    true,
	}
}

// AddPattern 添加一个模式（空串忽略，与 Python 版一致）。
func (ac *AhoCorasick) AddPattern(pattern string) {
	if pattern == "" {
		return
	}
	ac.built = false
	ac.revision++
	ac.patternSet[pattern] = struct{}{}
	state := 0
	for _, r := range pattern {
		next, ok := ac.nextStates[state][r]
		if !ok {
			next = len(ac.nextStates)
			ac.nextStates[state][r] = next
			ac.nextStates = append(ac.nextStates, map[rune]int{})
			ac.fail = append(ac.fail, 0)
			ac.output = append(ac.output, nil)
		}
		state = next
	}
	ac.output[state] = append(ac.output[state], pattern)
}

// AddPatterns 批量添加模式。
func (ac *AhoCorasick) AddPatterns(patterns []string) {
	for _, p := range patterns {
		ac.AddPattern(p)
	}
}

// Build 构建 fail 指针与输出合并（BFS）。
// 出处：core/utils/matcher.py L74-L93。
func (ac *AhoCorasick) Build() {
	if ac.built {
		return
	}
	queue := make([]int, 0, len(ac.nextStates))
	for _, state := range ac.nextStates[0] {
		queue = append(queue, state)
		ac.fail[state] = 0
	}
	for head := 0; head < len(queue); head++ {
		r := queue[head]
		for char, s := range ac.nextStates[r] {
			queue = append(queue, s)
			state := ac.fail[r]
			for {
				if _, ok := ac.nextStates[state][char]; ok || state == 0 {
					break
				}
				state = ac.fail[state]
			}
			if next, ok := ac.nextStates[state][char]; ok && next != s {
				ac.fail[s] = next
			} else {
				ac.fail[s] = 0
			}
			// 合并 fail 链上的输出。
			ac.output[s] = append(ac.output[s], ac.output[ac.fail[s]]...)
			ac.output[s] = dedupe(ac.output[s])
		}
	}
	ac.built = true
}

// MatchAll 返回文本中所有命中（按结束位置升序），大小写敏感。
func (ac *AhoCorasick) MatchAll(text string) []ACMatch {
	ac.Build()
	runes := []rune(text)
	state := 0
	var results []ACMatch
	for i, char := range runes {
		for {
			if _, ok := ac.nextStates[state][char]; ok || state == 0 {
				break
			}
			state = ac.fail[state]
		}
		if next, ok := ac.nextStates[state][char]; ok {
			state = next
		} else {
			state = 0
		}
		for _, pattern := range ac.output[state] {
			length := len([]rune(pattern))
			results = append(results, ACMatch{
				Pattern: pattern,
				Start:   i + 1 - length,
				End:     i + 1,
			})
		}
	}
	return results
}

// FindAll 返回每个命中模式的出现次数，对齐 Python find_all。
// 出处：core/utils/matcher.py L157-L171。
func (ac *AhoCorasick) FindAll(text string) map[string]int {
	stats := map[string]int{}
	for _, m := range ac.MatchAll(text) {
		stats[m.Pattern]++
	}
	return stats
}

// Len 返回模式总数。
func (ac *AhoCorasick) Len() int { return len(ac.patternSet) }

// Patterns 返回排序后的模式列表（便于确定性测试）。
func (ac *AhoCorasick) Patterns() []string {
	out := make([]string, 0, len(ac.patternSet))
	for p := range ac.patternSet {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// ExtractEntities 用 AC 匹配在文本里抽取实体，返回 {实体名: 出现次数} 权重。
// 对齐 Python DualPathRetriever._extract_entities（小写匹配 + 回映射原名 + 次数作权重）。
//
// 出处：core/retrieval/dual_path.py L3175-L3208。
func (ac *AhoCorasick) ExtractEntities(text string) map[string]float64 {
	if len(ac.patternSet) == 0 {
		return map[string]float64{}
	}
	stats := ac.FindAll(strings.ToLower(text))
	out := make(map[string]float64, len(stats))
	for pattern, count := range stats {
		out[pattern] = float64(count)
	}
	return out
}
