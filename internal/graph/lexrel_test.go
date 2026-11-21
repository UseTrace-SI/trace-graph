package graph

import (
	"fmt"
	"strings"
	"testing"
)

func importSummary(imports []lexImport) string {
	var parts []string
	for _, imp := range imports {
		parts = append(parts, fmt.Sprintf("%s:%s<%s|%s>", imp.kind, imp.local, imp.specifier, imp.imported))
	}
	return strings.Join(parts, " ")
}

func TestExtractImports(t *testing.T) {
	for _, tc := range []struct {
		name, lang, src, want string
	}{
		{"js default", "javascript", "import fs from 'fs';\n", "default:fs<fs|default>"},
		{"js named and alias", "typescript", "import { a, b as c } from './util';\n", "named:a<./util|a> named:c<./util|b>"},
		{"js namespace", "javascript", "import * as ns from \"../lib/x.js\";\n", "namespace:ns<../lib/x.js|>"},
		{"js side effect", "javascript", "import './side.css';\n", "module:<./side.css|>"},
		{"ts type-only", "typescript", "import type { T } from './types';\n", "named:T<./types|T>"},
		{"js default and named", "javascript", "import React, { useState } from 'react';\n", "default:React<react|default> named:useState<react|useState>"},
		{"js re-exports", "typescript", "export { x } from './x';\nexport * from './all';\nexport { y };\n", "reexport:x<./x|x> reexport:<./all|>"},
		{"js require", "javascript", "const y = require('./y');\nconst { p, q: r } = require('./pq');\n", "namespace:y<./y|> named:p<./pq|p> named:r<./pq|q>"},
		{"js dynamic import", "javascript", "async function f() { await import('./lazy'); loader.import('./not'); }\n", "module:<./lazy|>"},
		{"js multi-line", "typescript", "import {\n  one,\n  two,\n} from './multi';\n", "named:one<./multi|one> named:two<./multi|two>"},
		{"js comments and strings", "javascript", "// import { fake } from './commented';\nconst s = \"import { fake } from './string'\";\n/* import x from './block'; */\n", ""},
		{"py imports", "python", "import os\nimport a.b.c as m\nimport a.b\n", "module:os<os|> namespace:m<a.b.c|> module:a<a.b|>"},
		{"py from", "python", "from . import sibling\nfrom ..pkg.mod import f, g as h\nfrom y import *\n", "named:sibling<.|sibling> named:f<..pkg.mod|f> named:h<..pkg.mod|g> module:<y|>"},
		{"py parenthesized", "python", "from x import (\n    y,\n    z as w,\n)\n", "named:y<x|y> named:w<x|z>"},
		{"py commented", "python", "# import os\ns = 'from a import b'\n", ""},
		{"rust use", "rust", "use crate::a::b::Item;\nuse std::collections::HashMap;\n", "named:Item<crate::a::b|Item> named:HashMap<std::collections|HashMap>"},
		{"rust groups", "rust", "use super::{x, y::z as w};\nuse self::m::*;\nuse crate::a::b::{self, C};\n", "named:x<super|x> named:w<super::y|z> module:<self::m|> named:b<crate::a|b> named:C<crate::a::b|C>"},
		{"rust mod", "rust", "pub mod child;\nmod inline { }\n", "mod:child<self|child>"},
		{"java", "java", "import com.x.Y;\nimport static com.x.Y.m;\nimport com.x.*;\n", "named:Y<com.x|Y> named:m<com.x.Y|m> module:<com.x|>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := importSummary(extractImports(tc.lang, newLexSource(tc.lang, tc.src))); got != tc.want {
				t.Fatalf("imports %q, want %q", got, tc.want)
			}
		})
	}
}

func TestExtractRoutes(t *testing.T) {
	for _, tc := range []struct {
		name, lang, src, want string
	}{
		{"express", "javascript", "app.get('/users/:id', getUser);\nrouter.post(\"/items\", auth, controllers.create);\napp.delete('/x', (req, res) => res.end());\n", "GET /users/:id ->getUser POST /items controllers->create DELETE /x ->"},
		{"hono and methods", "typescript", "const app = new Hono();\napp.get('/hello', (c) => c.text('hi'));\napp.all('*', notFound);\nthis.router.put('/u', this.update);\n", "GET /hello -> ALL * ->notFound PUT /u this->update"},
		{"inline delegates", "typescript", "app.all('/a', (c) => roomsRoute(c.req.raw, c.env));\napp.get('/b', async (c) => await api.list(c));\napp.get('/c', (c) => { log(c); return run(c); });\napp.get('/d', (c) => first(c) || second(c));\napp.get('/e', (next) => next());\napp.all('/f', guard(['POST'], (c) => startPairing(c.req.raw)));\napp.get('/g', auth(handler));\napp.get('/h', outer(inner(handler)));\n", "ALL /a ->roomsRoute GET /b api->list GET /c -> GET /d -> GET /e -> ALL /f ->startPairing GET /g ->handler GET /h ->"},
		{"not routes", "javascript", "const v = params.get('id', 0);\ncache.get('/x');\nmap.set('/x', y);\napp.get(path, handler);\napp.get('/a' + id, handler);\n", ""},
		{"fastapi", "python", "@app.get(\"/items/{item_id}\")\nasync def read_item(item_id: int):\n    return {}\n", "GET /items/{item_id} ->read_item@2"},
		{"flask methods and stacked decorators", "python", "@bp.route(\"/login\", methods=[\"GET\", \"POST\"])\ndef login():\n    pass\n\n@app.route('/')\n@login_required\ndef index():\n    pass\n", "GET /login ->login@2 POST /login ->login@2 GET / ->index@7"},
		{"multi-line decorator", "python", "@router.post(\n    \"/x\",\n    status_code=201,\n)\ndef create():\n    pass\n", "POST /x ->create@5"},
		{"commented route", "python", "# @app.get('/hidden')\ndef hidden():\n    pass\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var parts []string
			for _, route := range extractRoutes(tc.lang, newLexSource(tc.lang, tc.src)) {
				part := fmt.Sprintf("%s %s %s->%s", route.method, route.path, route.qualifier, route.handler)
				if route.decorated > 0 {
					part += fmt.Sprintf("@%d", route.decorated)
				}
				parts = append(parts, part)
			}
			if got := strings.Join(parts, " "); got != tc.want {
				t.Fatalf("routes %q, want %q", got, tc.want)
			}
		})
	}
}
