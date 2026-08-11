// doclint は docs 配下の設計文書に対する機械チェックのSSOTである。
// ルールを追加・変更したら docs/_quality/STYLE_GUIDE.md を同じ変更で更新すること。
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type finding struct {
	severity string
	path     string
	line     int
	message  string
}

var (
	// 全角(かな・カナ・漢字・句読点)と半角英数の境界にある半角スペース
	jpClass    = `[ぁ-んァ-ヿ一-鿿、。]`
	boundaryRe = regexp.MustCompile(jpClass + ` [0-9A-Za-z]|[0-9A-Za-z] ` + jpClass)
	// 日本語文末のコロン
	colonEndRe = regexp.MustCompile(`[ぁ-ん一-鿿ァ-ヿ][:：]\s*$`)
	// アスタリスク2つの強調
	emphasisRe = regexp.MustCompile(`\*\*`)
	// 先送りマーカー
	tbdRe = regexp.MustCompile(`(?i)\bTBD\b|\bTODO\b|後で決める|未定とする|将来検討`)
	// 見出し・ラベル行(境界スペース検査の対象外)
	headingRe = regexp.MustCompile(`^#{1,6}\s|^作成日|^ステータス`)
	fenceRe   = regexp.MustCompile("^\\s*```")
	// 用語統一: 検出は文脈ガード付き(katakana/kanji複合語内では発火しない)
	termRules = []struct {
		re        *regexp.Regexp
		canonical string
	}{
		{regexp.MustCompile(`(?i)argon2(?:[^i]|$)`), "Argon2id"},
		{regexp.MustCompile(`(?i)postgres(?:[^q]|$)`), "PostgreSQL"},
	}
)

func lintFile(path string) []finding {
	// このツールは開発用のドキュメントLintであり、pathはfilepath.Walkから来る信頼された値
	data, err := os.ReadFile(path) // #nosec G304 -- 開発用ツール、ユーザ入力を直接受け取らない
	if err != nil {
		return []finding{{"Critical", path, 0, "読み取り失敗: " + err.Error()}}
	}
	var fs []finding
	inFence := false
	for i, line := range strings.Split(string(data), "\n") {
		n := i + 1
		if fenceRe.MatchString(line) {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		// インラインコード・URLは引用・識別子なので全ルールの検査対象から除外する
		stripped := regexp.MustCompile("`[^`]*`").ReplaceAllString(line, "")
		stripped = regexp.MustCompile(`https?://\S+`).ReplaceAllString(stripped, "")
		if emphasisRe.MatchString(stripped) {
			fs = append(fs, finding{"High", path, n, "アスタリスク2つの強調は使わない"})
		}
		if colonEndRe.MatchString(line) {
			fs = append(fs, finding{"Medium", path, n, "日本語文末のコロンは使わない"})
		}
		if tbdRe.MatchString(stripped) {
			fs = append(fs, finding{"Critical", path, n, "先送りマーカー(TBD/TODO/後で決める/未定とする/将来検討)は残さない"})
		}
		if !headingRe.MatchString(line) && boundaryRe.MatchString(stripped) {
			fs = append(fs, finding{"High", path, n, "日本語と英数字の境界に半角スペースを入れない: " + boundaryRe.FindString(stripped)})
		}
		for _, tr := range termRules {
			if tr.re.MatchString(stripped) && !strings.Contains(stripped, tr.canonical) {
				fs = append(fs, finding{"High", path, n, "用語統一: " + tr.canonical + " を使う"})
			}
		}
	}
	return fs
}

func main() {
	root := "docs"
	if len(os.Args) > 1 {
		root = os.Args[1]
	}
	var all []finding
	// 開発用ツールとして、rootはos.Argsから来る想定
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error { // #nosec G703 -- 開発用ツール
		if err != nil {
			return err
		}
		if !info.IsDir() && strings.HasSuffix(path, ".md") {
			all = append(all, lintFile(path)...)
		}
		return nil
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "walk error:", err)
		os.Exit(2)
	}
	counts := map[string]int{}
	for _, f := range all {
		counts[f.severity]++
		fmt.Printf("[%s] %s:%d %s\n", f.severity, f.path, f.line, f.message)
	}
	fmt.Printf("Critical %d / High %d / Medium %d / Low %d\n",
		counts["Critical"], counts["High"], counts["Medium"], counts["Low"])
	if counts["Critical"] > 0 || counts["High"] > 0 {
		os.Exit(1)
	}
}
