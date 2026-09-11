# api/openapi/v1.yaml is GENERATED from these protos (buf.gen.yaml) — the
# proto is the single source of truth for the HTTP surface, this file is never
# hand-edited. See http.proto for the annotation and cmd/protoc-gen-ignopenapi
# for the generator.
#
#   make lint         # buf lint + confirm the whole module compiles
#   make api-generate # regenerate api/openapi/v1.yaml in place
#   make api-check    # regenerate + fail if that changed anything (CI)
GO ?= go
PLUGIN := $(abspath ../../bin/protoc-gen-ignopenapi)

.PHONY: lint api-generate api-check

lint:
	buf lint
	buf build -o /dev/null

$(PLUGIN): ../../cmd/protoc-gen-ignopenapi/main.go
	$(GO) build -o $(PLUGIN) ../../cmd/protoc-gen-ignopenapi

api-generate: $(PLUGIN)
	PATH="$(dir $(PLUGIN)):$$PATH" buf generate

api-check: api-generate
	@git diff --exit-code -- ../openapi/v1.yaml || { \
	  echo "api/openapi/v1.yaml is stale — run 'make -C api/proto api-generate' and commit the result" >&2; \
	  exit 1; \
	}
