package gin_test

import "net/http"

func httpFileServer(dir string) http.Handler { return http.FileServer(http.Dir(dir)) }
