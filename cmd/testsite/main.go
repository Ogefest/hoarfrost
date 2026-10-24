// Command testsite serves the deterministic news site used by the test
// suite, for end-to-end runs with docker compose.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/ogefest/hoarfrost/internal/testsite"
)

func main() {
	addr := flag.String("addr", ":8000", "listen address")
	base := flag.String("base", "http://localhost:8000", "base URL used in the feeds")
	extra := flag.Int("articles", 5, "number of generated articles")
	flag.Parse()
	site := testsite.NewHandler(*base)
	for i := range *extra {
		site.AddArticle(fmt.Sprintf("/news/generated-%02d.html", i), fmt.Sprintf("Test article %d", i),
			strings.Repeat("Body of a generated test article about local affairs, investments and city council decisions. ", 6))
	}
	srv := &http.Server{Addr: *addr, Handler: site, ReadHeaderTimeout: 10 * time.Second}
	log.Printf("test site on %s (%s)", *addr, *base)
	log.Fatal(srv.ListenAndServe())
}
