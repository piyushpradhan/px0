package table

import (
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
)

func mustTable(t *testing.T, src string, delim rune) Result {
	t.Helper()
	res, err := Render(strings.NewReader(src), int64(len(src)), delim)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestTableQuotedCellWithCommaAndNewlineStaysOneCellAndLinesSkip(t *testing.T) {
	t.Parallel()
	res := mustTable(t, "repo,note\nchromium,\"monorepo,\ndepot_tools excluded\"\nvscode,warm\n", ',')
	if got := res.Rows[0].Cells[1]; got != "monorepo,\ndepot_tools excluded" {
		t.Errorf("quoted cell = %q", got)
	}
	if res.HeaderLine != 1 || res.Rows[0].Line != 2 || res.Rows[1].Line != 4 {
		t.Errorf("lines = header %d, rows %d %d; want 1, 2 4", res.HeaderLine, res.Rows[0].Line, res.Rows[1].Line)
	}
}

func TestTableRaggedRowsKeepTheirCellsAndWidenTheTable(t *testing.T) {
	t.Parallel()
	res := mustTable(t, "a,b,c\n1\n1,2,3,4\n", ',')
	if len(res.Rows[0].Cells) != 1 || len(res.Rows[1].Cells) != 4 {
		t.Errorf("rows = %v", res.Rows)
	}
	if res.Cols != 4 {
		t.Errorf("cols = %d, want 4", res.Cols)
	}
}

func TestTableTSVSplitsOnTabsOnly(t *testing.T) {
	t.Parallel()
	res := mustTable(t, "name\tcity\nDoe, Jane\tLeeds\n", '\t')
	if want := []string{"Doe, Jane", "Leeds"}; !reflect.DeepEqual(res.Rows[0].Cells, want) {
		t.Errorf("cells = %q, want %q", res.Rows[0].Cells, want)
	}
}

func TestTableStripsByteOrderMarkFromHeader(t *testing.T) {
	t.Parallel()
	res := mustTable(t, "\ufeffid,name\n1,x\n", ',')
	if res.Header[0] != "id" {
		t.Errorf("header[0] = %q", res.Header[0])
	}
}

func TestTableBlankLinesAreSkippedButLineNumbersStayTrue(t *testing.T) {
	t.Parallel()
	res := mustTable(t, "a,b\n\n1,2\n\n\n3,4\n", ',')
	if len(res.Rows) != 2 || res.Rows[0].Line != 3 || res.Rows[1].Line != 6 {
		t.Errorf("rows = %+v", res.Rows)
	}
}

func TestTableStrayQuotesDoNotFailTheFile(t *testing.T) {
	t.Parallel()
	res := mustTable(t, "a,b\n5\" pipe,x\n", ',')
	if res.Rows[0].Cells[0] != "5\" pipe" {
		t.Errorf("cell = %q", res.Rows[0].Cells[0])
	}
}

func TestTableEmptyFileHasNoColumns(t *testing.T) {
	t.Parallel()
	res := mustTable(t, "", ',')
	if res.Cols != 0 || len(res.Rows) != 0 || res.Truncated {
		t.Errorf("res = %+v", res)
	}
}

func TestTableStopsAtRowCap(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	b.WriteString("n,sq\n")
	for i := 1; i <= MaxRows+1000; i++ {
		fmt.Fprintf(&b, "%d,%d\n", i, i*i)
	}
	res := mustTable(t, b.String(), ',')
	if len(res.Rows) != MaxRows || !res.Truncated {
		t.Errorf("rows %d truncated %v; want %d true", len(res.Rows), res.Truncated, MaxRows)
	}
	if last := res.Rows[len(res.Rows)-1]; last.Line != MaxRows+1 {
		t.Errorf("last row line %d", last.Line)
	}
}

func TestTableExactlyAtRowCapIsNotTruncated(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	b.WriteString("n\n")
	for i := 1; i <= MaxRows; i++ {
		fmt.Fprintf(&b, "%d\n", i)
	}
	if res := mustTable(t, b.String(), ','); res.Truncated {
		t.Error("a file of exactly the cap should not say it was cut")
	}
}

type rowSource struct {
	row  string
	off  int
	read int64
}

func (s *rowSource) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		c := copy(p[n:], s.row[s.off:])
		n += c
		s.off = (s.off + c) % len(s.row)
	}
	s.read += int64(n)
	return n, nil
}

func TestTableReadsNoMoreThanTheByteCap(t *testing.T) {
	t.Parallel()
	src := &rowSource{row: strings.Repeat("y", 10000) + ",end\n"}
	res, err := Render(io.MultiReader(strings.NewReader("a,b\n"), src), 50<<20, ',')
	if err != nil {
		t.Fatal(err)
	}
	if src.read > MaxBytes {
		t.Errorf("read %d bytes of a 50 MB file, cap is %d", src.read, MaxBytes)
	}
	if !res.Truncated || len(res.Rows) == 0 {
		t.Fatalf("truncated %v with %d rows", res.Truncated, len(res.Rows))
	}
	for _, r := range res.Rows {
		if len(r.Cells) != 2 || r.Cells[1] != "end" {
			t.Fatalf("the record the byte cap cut through reached the result: %d cells", len(r.Cells))
		}
	}
}

func TestTableFileAtTheByteCapIsNotTruncated(t *testing.T) {
	t.Parallel()
	src := "a,b\n1,2\n"
	res, err := Render(strings.NewReader(src), MaxBytes, ',')
	if err != nil || res.Truncated {
		t.Errorf("truncated %v, err %v", res.Truncated, err)
	}
}

func TestTableDelimiterComesFromTheFileNotTheExtension(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		src      string
		fallback rune
		want     []string
	}{
		"tab-separated .csv":            {"date\tplace\tnote\n2018\tLondon\tx, y\n", ',', []string{"2018", "London", "x, y"}},
		"semicolon .csv":                {"a;b;c\n1,5;2;3\n", ',', []string{"1,5", "2", "3"}},
		"pipe .csv":                     {"a|b\n1|2\n", ',', []string{"1", "2"}},
		"comma .tsv":                    {"a,b\n1,2\n", '\t', []string{"1", "2"}},
		"quoted commas ignored":         {"\"a,b,c\"\tz\n1\t2\n", ',', []string{"1", "2"}},
		"blank first line":              {"\n\na\tb\n1\t2\n", ',', []string{"1", "2"}},
		"single column keeps extension": {"name\nx\n", ',', []string{"x"}},
	} {
		res, err := Render(strings.NewReader(tc.src), int64(len(tc.src)), tc.fallback)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(res.Rows) == 0 || !reflect.DeepEqual(res.Rows[0].Cells, tc.want) {
			t.Errorf("%s: rows = %+v, want first row %q", name, res.Rows, tc.want)
		}
	}
}

func TestTableTieBetweenSeparatorsKeepsTheExtension(t *testing.T) {
	t.Parallel()
	if got := SniffDelim([]byte("a,b\tc\n"), '\t'); got != '\t' {
		t.Errorf("tie picked %q, want the .tsv tab", got)
	}
	if got := SniffDelim([]byte("a,b\tc\n"), ','); got != ',' {
		t.Errorf("tie picked %q, want the .csv comma", got)
	}
}

type errReader struct {
	failAt int
	count  int
}

func (e *errReader) Read(p []byte) (int, error) {
	if e.count >= e.failAt {
		return 0, io.ErrUnexpectedEOF
	}
	e.count++
	return copy(p, "col1,col2\n"), nil
}

func TestTableHelpersAndErrorBranches(t *testing.T) {
	t.Parallel()

	// 1. Delim and IsTable
	d, ok := Delim("file.csv")
	if !ok || d != ',' {
		t.Errorf("expected comma for .csv, got %c, %v", d, ok)
	}
	d, ok = Delim("file.tsv")
	if !ok || d != '\t' {
		t.Errorf("expected tab for .tsv, got %c, %v", d, ok)
	}
	d, ok = Delim("file.txt")
	if ok || d != 0 {
		t.Errorf("expected no delim for .txt, got %c, %v", d, ok)
	}

	if !IsTable("data.csv") || !IsTable("data.tsv") || IsTable("data.json") {
		t.Errorf("unexpected IsTable result")
	}

	// 2. SniffDelim with no newline in buffer
	if delim := SniffDelim([]byte("a,b,c"), ','); delim != ',' {
		t.Errorf("expected comma without newline, got %c", delim)
	}

	// 3. Header read error (!cut)
	_, err := Render(&errReader{failAt: 0}, 100, ',')
	if err == nil {
		t.Error("expected error on header read failure")
	}

	// 4. Row read error (!cut)
	_, err = Render(&errReader{failAt: 1}, 100, ',')
	if err == nil {
		t.Error("expected error on row read failure without cut")
	}

	// 5. Row read error (cut)
	res, err := Render(&errReader{failAt: 1}, MaxBytes+100, ',')
	if err != nil {
		t.Errorf("unexpected error on cut row failure: %v", err)
	}
	if !res.Truncated {
		t.Error("expected truncated result on cut")
	}
}

func BenchmarkRenderTable(b *testing.B) {
	src := `id,name,role,department,location,salary
1,Alice,Engineer,Core,New York,150000
2,Bob,Designer,UI,San Francisco,140000
3,Charlie,Product,Core,Remote,160000
4,Dana,DevOps,Infra,London,155000
`
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = Render(strings.NewReader(src), int64(len(src)), ',')
	}
}

func BenchmarkSniffDelim(b *testing.B) {
	data := []byte("id,name,role,department,location,salary\n1,Alice,Engineer,Core,New York,150000\n")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = SniffDelim(data, ',')
	}
}

