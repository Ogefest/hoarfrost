package upload

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"mime/quotedprintable"
	"strings"
)

func base64Reader(r io.Reader) io.Reader {
	return base64.NewDecoder(base64.StdEncoding, stripNewlines{r})
}

func qpReader(r io.Reader) io.Reader { return quotedprintable.NewReader(r) }

type stripNewlines struct{ r io.Reader }

func (s stripNewlines) Read(p []byte) (int, error) {
	for {
		n, err := s.r.Read(p)
		if n == 0 {
			return 0, err
		}
		out := strings.NewReplacer("\r", "", "\n", "", " ", "").Replace(string(p[:n]))
		if len(out) == 0 && err == nil {
			continue
		}
		copy(p, out)
		return len(out), err
	}
}

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }
