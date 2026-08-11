// crapcheck 関数ごとのCRAPスコアを計算し閾値超過を検出します。
// CRAP = complexity^2 * (1 - coverage)^3 + complexity
// - complexity: gocycloが返す循環的複雑度
// - coverage: 関数ごとのカバレッジ（0.0〜1.0）
// カバレッジは `go test -coverprofile=... ./...` のプロファイルを `go tool cover -func=...` で関数別に集計したものを使います。
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

type funcCov struct {
	file     string
	line     int
	function string
	coverage float64 // 0.0〜1.0
}

type funcComplexity struct {
	pkg     string
	name    string
	file    string
	line    int
	complex int
}

// funcKey (file, line, function) の組で厳密照合
type funcKey struct {
	file     string
	function string
	line     int
}

func main() {
	profile := flag.String("profile", "coverage.out", "go test の -coverprofile 出力")
	targetPath := flag.String("path", "./internal/...", "gocycloで解析する対象パス（複数指定可、カンマ区切り）")
	threshold := flag.Float64("threshold", 15.0, "CRAP閾値（超過があれば非ゼロ終了）")
	excludePrefix := flag.String("exclude-prefix", "internal/platform/sqlc", "対象外にするファイルパス接頭辞（カンマ区切り）")
	flag.Parse()

	covs, err := loadCoverages(*profile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "coverage読込失敗:", err)
		os.Exit(2)
	}

	targets := strings.Split(*targetPath, ",")
	comps, err := runGocyclo(targets)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gocyclo実行失敗:", err)
		os.Exit(2)
	}

	excludes := splitAndTrim(*excludePrefix)

	violations := 0
	fmt.Println("complexity coverage crap function file:line")
	for _, c := range comps {
		if isExcluded(c.file, excludes) {
			continue
		}
		key := funcKey{file: c.file, function: c.name, line: c.line}
		cov, ok := lookupCoverage(covs, key, c)
		if !ok {
			// カバレッジ情報がない関数（未実行 or テスト対象外）はcov=0で計算
			cov = 0
		}
		inv := 1 - cov
		crap := float64(c.complex*c.complex)*(inv*inv*inv) + float64(c.complex)
		flag := ""
		if crap >= *threshold {
			flag = "  BAD"
			violations++
		}
		fmt.Printf("%d %.3f %.3f %s.%s %s:%d%s\n", c.complex, cov, crap, c.pkg, c.name, c.file, c.line, flag)
	}
	if violations > 0 {
		fmt.Fprintf(os.Stderr, "CRAP閾値 %.1f を超えた関数: %d件\n", *threshold, violations)
		os.Exit(1)
	}
}

func loadCoverages(path string) ([]funcCov, error) {
	// go tool cover -func=<profile> の出力をパース
	// 開発ツールとして固定コマンドを実行する
	out, err := exec.Command("go", "tool", "cover", "-func="+path).Output() // #nosec G204 -- 開発用ツール
	if err != nil {
		return nil, err
	}
	var covs []funcCov
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "total:") {
			continue
		}
		// 形式: "path/to/file.go:LINE:\tFUNC\tCOV%"
		// タブ区切りだがスペース区切りにも寛容にする
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		loc := fields[0] // file:line:
		fn := fields[1]
		covStr := strings.TrimSuffix(fields[2], "%")
		cov, err := strconv.ParseFloat(covStr, 64)
		if err != nil {
			continue
		}
		file, lineNo := parseLoc(loc)
		covs = append(covs, funcCov{file: file, line: lineNo, function: fn, coverage: cov / 100.0})
	}
	return covs, sc.Err()
}

// parseLoc `FILE:LINE:` （coverの形式）または `FILE:LINE:COL` （gocycloの形式）から
// ファイル名と1始まりの行番号を取り出します。
// 末尾コロンは除去後、末尾から数値要素を1つ、次に手前が数値ならさらに1つと消費し、
// 数値でない要素を含むまでをファイル名とみなします。行番号は末尾から2番目の数値、無ければ最後の数値です。
func parseLoc(loc string) (string, int) {
	loc = strings.TrimSuffix(loc, ":")
	parts := strings.Split(loc, ":")
	// 末尾から連続する数値要素を集める
	tailInts := []int{}
	cut := len(parts)
	for i := len(parts) - 1; i >= 1; i-- {
		n, err := strconv.Atoi(parts[i])
		if err != nil {
			break
		}
		tailInts = append([]int{n}, tailInts...) // 元の並びに戻す
		cut = i
	}
	if len(tailInts) == 0 {
		return loc, 0
	}
	line := tailInts[0] // 2つ以上あれば最初 (=LINE)、1つならその値 (=LINE)
	return strings.Join(parts[:cut], ":"), line
}

func runGocyclo(targets []string) ([]funcComplexity, error) {
	// gocycloはGoパターン（./foo/...）をサポートしないためディレクトリに正規化する
	normalized := make([]string, 0, len(targets))
	for _, t := range targets {
		t = strings.TrimSpace(t)
		t = strings.TrimSuffix(t, "/...")
		t = strings.TrimPrefix(t, "./")
		if t == "" {
			t = "."
		}
		normalized = append(normalized, t)
	}
	args := append([]string{"-avg"}, normalized...)
	// 開発用ツールで、gocycloの引数は自身が生成する
	cmd := exec.Command("gocyclo", args...) // #nosec G204 -- 開発用ツール
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("gocyclo %v: %w", args, err)
	}
	// 出力形式: "COMPLEX PKG FUNC FILE:LINE:1"
	var comps []funcComplexity
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "Average") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		comp, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		pkg := fields[1]
		name := fields[2]
		loc := fields[3]
		file, lineNo := parseLoc(loc)
		// テストファイルは除外
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		// gocyclo は "(*Type).Method" や "(Type).Method" 形式でメソッドを返すが、
		// go tool cover -func はメソッド名だけを返すため、正規化する
		name = stripReceiver(name)
		comps = append(comps, funcComplexity{pkg: pkg, name: name, file: file, line: lineNo, complex: comp})
	}
	return comps, sc.Err()
}

// lookupCoverage `go tool cover -func` はモジュールパス付きの絶対相対パス（例 github.com/x/y/pkg/f.go）
// を返す一方、gocycloはワーキングディレクトリ相対（例 internal/pkg/f.go）です。
// 関数名と、ファイル名末尾（basename）+ 開始行の完全一致で照合します。
func lookupCoverage(covs []funcCov, _ funcKey, c funcComplexity) (float64, bool) {
	baseC := filepath.Base(c.file)
	for _, cv := range covs {
		if cv.function != c.name {
			continue
		}
		if filepath.Base(cv.file) != baseC {
			continue
		}
		if cv.line != c.line {
			continue
		}
		return cv.coverage, true
	}
	return 0, false
}

func isExcluded(path string, prefixes []string) bool {
	for _, p := range prefixes {
		if p == "" {
			continue
		}
		if strings.Contains(path, p) {
			return true
		}
	}
	return false
}

// stripReceiver "(*Type).Method" や "(Type).Method" からメソッド名だけを取り出す
func stripReceiver(name string) string {
	if strings.HasPrefix(name, "(") {
		if _, rest, ok := strings.Cut(name, ")."); ok {
			return rest
		}
	}
	return name
}

func splitAndTrim(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
