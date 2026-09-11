// Command protoc-gen-ignopenapi is a protoc plugin that generates the Ignition
// v1 OpenAPI v3 document (api/openapi/v1.yaml) from the protobuf contracts in
// api/proto/ignition/v1, using the HTTP-mapping annotation declared in
// http.proto (custom extension field 50000 on MethodOptions).
//
// It is self-contained: it depends only on the Go protobuf runtime and
// gopkg.in/yaml.v3 (both already in the module graph), so it builds and runs
// offline with no remote plugins and no googleapis dependency.
//
// The proto is the single source of truth for the HTTP API surface: field
// names/types/enums come from the .proto messages, and routes/bodies/status
// codes come from the (ignition.v1.http) annotation. Running the generator
// reproduces api/openapi/v1.yaml byte-for-byte, which is enforced by
// `make api-check`. Behavioral constraints not expressible in proto3 (which
// fields are required, numeric ranges) are documented in
// docs/design/ignition-api-contract.md and validated by internal/api tests.
package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"unicode"
	"unicode/utf8"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/pluginpb"
	"gopkg.in/yaml.v3"
)

// httpExt is the custom-option field number of HttpRule on MethodOptions
// (declared by `extend google.protobuf.MethodOptions { HttpRule http = 50000; }`
// in ignition/v1/http.proto).
const httpExt = 50000

const (
	pkgIgnition = "ignition.v1"
	tsType      = ".google.protobuf.Timestamp"
	structType  = ".google.protobuf.Struct"
)

// httpRule is the decoded HTTP mapping for an RPC.
type httpRule struct {
	method string
	path   string
	body   string
	code   int32
}

// parseHTTPRule reads the (ignition.v1.http) custom option out of a
// MethodOptions message. The Go protobuf runtime does not know the option's
// type, so the bytes are preserved in the message's unknown fields and decoded
// here with the raw wire format.
func parseHTTPRule(opts *descriptorpb.MethodOptions) (*httpRule, bool) {
	if opts == nil {
		return nil, false
	}
	b := opts.ProtoReflect().GetUnknown()
	var hr httpRule
	found := false
	for len(b) > 0 {
		num, wt, tagN := protowire.ConsumeTag(b)
		if tagN < 0 {
			return nil, false
		}
		b = b[tagN:]
		if wt == protowire.BytesType {
			val, n := protowire.ConsumeBytes(b)
			if n < 0 {
				return nil, false
			}
			b = b[n:]
			if num == httpExt {
				found = true
				decodeHTTPRule(val, &hr)
			}
			continue
		}
		n := protowire.ConsumeFieldValue(num, wt, b)
		if n < 0 {
			return nil, false
		}
		b = b[n:]
	}
	if !found {
		return nil, false
	}
	return &hr, true
}

// decodeHTTPRule decodes the inner message of an HttpRule
// (method=1 path=2 body=3 code=4).
func decodeHTTPRule(b []byte, hr *httpRule) {
	for len(b) > 0 {
		num, wt, tagN := protowire.ConsumeTag(b)
		if tagN < 0 {
			return
		}
		b = b[tagN:]
		switch wt {
		case protowire.BytesType:
			val, n := protowire.ConsumeString(b)
			if n < 0 {
				return
			}
			b = b[n:]
			switch num {
			case 1:
				hr.method = val
			case 2:
				hr.path = val
			case 3:
				hr.body = val
			}
		case protowire.VarintType:
			val, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return
			}
			b = b[n:]
			if num == 4 {
				hr.code = int32(val)
			}
		default:
			n := protowire.ConsumeFieldValue(num, wt, b)
			if n < 0 {
				return
			}
			b = b[n:]
		}
	}
}

// --------------------------------------------------------------------------
// Index
// --------------------------------------------------------------------------

type index struct {
	messages map[string]*descriptorpb.DescriptorProto
	enums    map[string]*descriptorpb.EnumDescriptorProto
	pkg      map[string]string // type name -> package
}

func buildIndex(files []*descriptorpb.FileDescriptorProto) *index {
	ix := &index{
		messages: make(map[string]*descriptorpb.DescriptorProto),
		enums:    make(map[string]*descriptorpb.EnumDescriptorProto),
		pkg:      make(map[string]string),
	}
	for _, f := range files {
		pkg := f.GetPackage()
		prefix := "." + pkg
		for _, e := range f.GetEnumType() {
			ix.enums[prefix+"."+e.GetName()] = e
			ix.pkg[prefix+"."+e.GetName()] = pkg
		}
		for _, m := range f.GetMessageType() {
			walkMessage(prefix, m, ix, pkg)
		}
	}
	return ix
}

func walkMessage(prefix string, m *descriptorpb.DescriptorProto, ix *index, pkg string) {
	full := prefix + "." + m.GetName()
	ix.messages[full] = m
	ix.pkg[full] = pkg
	for _, e := range m.GetEnumType() {
		fe := full + "." + e.GetName()
		ix.enums[fe] = e
		ix.pkg[fe] = pkg
	}
	for _, nested := range m.GetNestedType() {
		walkMessage(full, nested, ix, pkg)
	}
}

// --------------------------------------------------------------------------
// OpenAPI model
// --------------------------------------------------------------------------

// nullableType marshals to an OpenAPI 3.1 flow-style type union, e.g.
// `type: [string, "null"]`.
type nullableType struct{ base string }

func (n nullableType) MarshalYAML() (any, error) {
	seq := &yaml.Node{Kind: yaml.SequenceNode, Style: yaml.FlowStyle}
	seq.Content = append(seq.Content, scalarNode(n.base))
	seq.Content = append(seq.Content, nullNode())
	return seq, nil
}

func scalarNode(v string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v}
}

func nullNode() *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "null"}
}

// Schema is a minimal OpenAPI 3.1 JSON-schema fragment.
type Schema struct {
	Type                 any                `yaml:"type,omitempty"`
	Format               string             `yaml:"format,omitempty"`
	Ref                  string             `yaml:"$ref,omitempty"`
	Enum                 []string           `yaml:"enum,omitempty"`
	Items                *Schema            `yaml:"items,omitempty"`
	Properties           map[string]*Schema `yaml:"properties,omitempty"`
	AdditionalProperties *Schema            `yaml:"additionalProperties,omitempty"` // nil == omit
	Description          string             `yaml:"description,omitempty"`
}

func refSchema(name string) *Schema { return &Schema{Ref: "#/components/schemas/" + name} }

type parameter struct {
	Name        string  `yaml:"name"`
	In          string  `yaml:"in"`
	Description string  `yaml:"description,omitempty"`
	Required    bool    `yaml:"required"`
	Schema      *Schema `yaml:"schema"`
}

type contentEntry struct {
	Schema *Schema `yaml:"schema"`
}

type response struct {
	Description string                  `yaml:"description"`
	Content     map[string]contentEntry `yaml:"content,omitempty"`
}

type operation struct {
	OperationID string              `yaml:"operationId"`
	Summary     string              `yaml:"summary,omitempty"`
	Parameters  []parameter         `yaml:"parameters,omitempty"`
	RequestBody *requestBody        `yaml:"requestBody,omitempty"`
	Responses   map[string]response `yaml:"responses"`
}

type requestBody struct {
	Required bool                    `yaml:"required"`
	Content  map[string]contentEntry `yaml:"content"`
}

// pathItem holds the GET/POST/PUT/DELETE for a path, plus shared params.
type pathItem struct {
	Parameters []parameter `yaml:"parameters,omitempty"`
	Get        *operation  `yaml:"get,omitempty"`
	Post       *operation  `yaml:"post,omitempty"`
	Delete     *operation  `yaml:"delete,omitempty"`
	Put        *operation  `yaml:"put,omitempty"`
}

type infoDoc struct {
	Title       string `yaml:"title"`
	Version     string `yaml:"version"`
	Description string `yaml:"description"`
}

type serverDoc struct {
	URL string `yaml:"url"`
}

type componentsDoc struct {
	SecuritySchemes map[string]map[string]any `yaml:"securitySchemes"`
	Parameters      map[string]parameter      `yaml:"parameters"`
	Schemas         map[string]*Schema        `yaml:"schemas"`
}

type openapiDoc struct {
	Openapi    string                `yaml:"openapi"`
	Info       infoDoc               `yaml:"info"`
	Servers    []serverDoc           `yaml:"servers"`
	Security   []map[string][]string `yaml:"security"`
	Paths      map[string]pathItem   `yaml:"paths"`
	Components componentsDoc         `yaml:"components"`
}

// --------------------------------------------------------------------------
// Generation
// --------------------------------------------------------------------------

func generate(req *pluginpb.CodeGeneratorRequest) (*pluginpb.CodeGeneratorResponse, error) {
	ix := buildIndex(req.GetProtoFile())

	const infoDesc = "HTTP/JSON view of api/proto/ignition/v1 (SandboxService + OperationService). " +
		"Process create/get/list/attach/signal/cancel are on SandboxService. " +
		"Exec bytes travel on ignition-gateway after AttachProcess. " +
		"Implemented behavior: docs/guides/ignition-implementation.md. " +
		"Full v1 contract: docs/design/ignition-api-contract.md."

	doc := &openapiDoc{
		Openapi:  "3.1.0",
		Info:     infoDoc{Title: "Ignition API", Version: "0.1.0", Description: infoDesc},
		Servers:  []serverDoc{{URL: "https://api.ignition.dev"}},
		Security: []map[string][]string{{"bearerAuth": {}}},
		Paths:    map[string]pathItem{},
		Components: componentsDoc{
			SecuritySchemes: map[string]map[string]any{
				"bearerAuth": {"type": "http", "scheme": "bearer", "bearerFormat": "RFC 9068 JWT"},
			},
			Parameters: map[string]parameter{
				"ProjectId":      {Name: "project", In: "path", Required: true, Schema: &Schema{Type: "string"}},
				"SandboxId":      {Name: "sandbox", In: "path", Required: true, Schema: &Schema{Type: "string"}},
				"OperationId":    {Name: "operation", In: "path", Required: true, Schema: &Schema{Type: "string"}},
				"ProcessId":      {Name: "process", In: "path", Required: true, Schema: &Schema{Type: "string"}},
				"IdempotencyKey": {Name: "Idempotency-Key", In: "header", Required: true, Schema: &Schema{Type: "string"}, Description: "Opaque client-provided key for idempotent retries; required for all mutations."},
			},
			Schemas: map[string]*Schema{},
		},
	}

	// Component schemas for every ignition.v1 message.
	for _, f := range req.GetProtoFile() {
		if f.GetPackage() != pkgIgnition {
			continue
		}
		for _, m := range f.GetMessageType() {
			addMessageSchema(doc, m, ix)
		}
	}

	// Paths from annotated RPCs.
	for _, f := range req.GetProtoFile() {
		if f.GetPackage() != pkgIgnition {
			continue
		}
		for _, svc := range f.GetService() {
			for _, m := range svc.GetMethod() {
				rule, ok := parseHTTPRule(m.GetOptions())
				if !ok {
					continue
				}
				addPath(doc, m, rule, ix)
			}
		}
	}

	body, err := marshalOpenAPI(doc)
	if err != nil {
		return nil, err
	}

	header := "# This file is generated from api/proto/ignition/v1 by cmd/protoc-gen-ignopenapi.\n" +
		"# Do not edit by hand: run `make api-generate` (or `buf generate` from api/proto).\n" +
		"# Behavioral constraints not expressible in proto3 (required fields, numeric\n" +
		"# ranges) live in docs/design/ignition-api-contract.md and are enforced by\n" +
		"# the ignition-api implementation and its tests.\n\n"

	return &pluginpb.CodeGeneratorResponse{
		// Advertise proto3-optional support so buf does not warn when
		// generating from files that use `optional` fields (process.proto).
		SupportedFeatures: proto.Uint64(uint64(pluginpb.CodeGeneratorResponse_FEATURE_PROTO3_OPTIONAL)),
		File: []*pluginpb.CodeGeneratorResponse_File{{
			Name:    ptrString("v1.yaml"),
			Content: ptrString(header + string(body)),
		}},
	}, nil
}

func addMessageSchema(doc *openapiDoc, m *descriptorpb.DescriptorProto, ix *index) {
	name := m.GetName()
	// Skip proto-internal synthetic types that are not part of the API
	// contract: map-entry messages (OpenAPI models maps via
	// additionalProperties) and the HttpRule annotation message (a tooling
	// detail of http.proto, not a wire type).
	if m.GetOptions().GetMapEntry() || name == "HttpRule" {
		for _, nested := range m.GetNestedType() {
			addMessageSchema(doc, nested, ix)
		}
		return
	}
	doc.Components.Schemas[name] = messageSchema(m, ix)
	for _, nested := range m.GetNestedType() {
		addMessageSchema(doc, nested, ix)
	}
}

func messageSchema(m *descriptorpb.DescriptorProto, ix *index) *Schema {
	props := make(map[string]*Schema, len(m.GetField()))
	for _, f := range m.GetField() {
		jsonName := f.GetJsonName()
		if jsonName == "" {
			jsonName = toLowerCamel(f.GetName())
		}
		props[jsonName] = schemaForField(f, ix)
	}
	if len(props) == 0 {
		props = nil
	}
	return &Schema{Type: "object", Properties: props}
}

// schemaForField builds an OpenAPI schema for a field, handling maps, repeated,
// enums, messages, and scalars.
func schemaForField(f *descriptorpb.FieldDescriptorProto, ix *index) *Schema {
	// Map field: type name is a synthetic MapEntry message.
	if f.GetLabel() == descriptorpb.FieldDescriptorProto_LABEL_REPEATED &&
		f.GetType() == descriptorpb.FieldDescriptorProto_TYPE_MESSAGE {
		if me := ix.messages[f.GetTypeName()]; me != nil && me.GetOptions().GetMapEntry() {
			val := fieldByNumber(me, 2) // MapValue
			addl := &Schema{}           // freeform default
			if val != nil {
				addl = scalarFieldSchema(val, ix)
			}
			return &Schema{Type: "object", AdditionalProperties: addl}
		}
	}
	if f.GetLabel() == descriptorpb.FieldDescriptorProto_LABEL_REPEATED {
		return &Schema{Type: "array", Items: scalarFieldSchema(f, ix)}
	}
	return scalarFieldSchema(f, ix)
}

func scalarFieldSchema(f *descriptorpb.FieldDescriptorProto, ix *index) *Schema {
	switch f.GetType() {
	case descriptorpb.FieldDescriptorProto_TYPE_ENUM:
		return enumFieldSchema(f, ix)
	case descriptorpb.FieldDescriptorProto_TYPE_MESSAGE:
		return messageFieldSchema(f, ix)
	case descriptorpb.FieldDescriptorProto_TYPE_STRING:
		return &Schema{Type: "string"}
	case descriptorpb.FieldDescriptorProto_TYPE_BYTES:
		return &Schema{Type: "string", Format: "byte"}
	case descriptorpb.FieldDescriptorProto_TYPE_BOOL:
		return &Schema{Type: "boolean"}
	case descriptorpb.FieldDescriptorProto_TYPE_DOUBLE,
		descriptorpb.FieldDescriptorProto_TYPE_FLOAT,
		descriptorpb.FieldDescriptorProto_TYPE_INT32,
		descriptorpb.FieldDescriptorProto_TYPE_INT64,
		descriptorpb.FieldDescriptorProto_TYPE_UINT32,
		descriptorpb.FieldDescriptorProto_TYPE_UINT64,
		descriptorpb.FieldDescriptorProto_TYPE_SINT32,
		descriptorpb.FieldDescriptorProto_TYPE_SINT64,
		descriptorpb.FieldDescriptorProto_TYPE_FIXED32,
		descriptorpb.FieldDescriptorProto_TYPE_FIXED64,
		descriptorpb.FieldDescriptorProto_TYPE_SFIXED32,
		descriptorpb.FieldDescriptorProto_TYPE_SFIXED64:
		return &Schema{Type: "integer"}
	default:
		return &Schema{Type: "string"}
	}
}

func messageFieldSchema(f *descriptorpb.FieldDescriptorProto, _ *index) *Schema {
	switch f.GetTypeName() {
	case tsType:
		return &Schema{Type: nullableType{"string"}, Format: "date-time"}
	case structType:
		return &Schema{Type: nullableType{"object"}, AdditionalProperties: &Schema{}}
	default:
		return refSchema(messageSimpleName(f.GetTypeName()))
	}
}

func enumFieldSchema(f *descriptorpb.FieldDescriptorProto, ix *index) *Schema {
	e := ix.enums[f.GetTypeName()]
	if e == nil {
		return &Schema{Type: "string"}
	}
	simple := enumTypeName(f.GetTypeName())
	vals := make([]string, 0, len(e.GetValue()))
	for _, v := range e.GetValue() {
		if strings.HasSuffix(v.GetName(), "_UNSPECIFIED") {
			continue
		}
		vals = append(vals, stripEnumPrefix(simple, v.GetName()))
	}
	return &Schema{Type: "string", Enum: vals}
}

func messageSimpleName(name string) string {
	name = strings.TrimPrefix(name, ".")
	parts := strings.Split(name, ".")
	return parts[len(parts)-1]
}

func enumTypeName(name string) string {
	name = strings.TrimPrefix(name, ".")
	parts := strings.Split(name, ".")
	return parts[len(parts)-1]
}

// toScreamingSnake converts a CamelCase proto type name to SCREAMING_SNAKE_CASE,
// e.g. "SandboxState" -> "SANDBOX_STATE". It is the prefix stripped from enum
// values so that ACCELERATOR_TYPE_NVIDIA_L4 becomes "NVIDIA_L4".
func toScreamingSnake(camel string) string {
	var b strings.Builder
	for i, r := range camel {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			b.WriteRune(unicode.ToLower(r))
		} else {
			b.WriteRune(r)
		}
	}
	return strings.ToUpper(b.String())
}

// stripEnumPrefix removes the "<ENUM_TYPE>_" prefix from an enum value, e.g.
// "SANDBOX_STATE_CREATING" with type "SandboxState" -> "CREATING".
func stripEnumPrefix(typeName, value string) string {
	return strings.TrimPrefix(value, toScreamingSnake(typeName)+"_")
}

func fieldByNumber(m *descriptorpb.DescriptorProto, n int32) *descriptorpb.FieldDescriptorProto {
	for _, f := range m.GetField() {
		if f.GetNumber() == n {
			return f
		}
	}
	return nil
}

// --------------------------------------------------------------------------
// OpenAPI assembly helpers
// --------------------------------------------------------------------------

var pathParamComponents = map[string]string{ // {param-name} -> component param name
	"project":   "ProjectId",
	"sandbox":   "SandboxId",
	"operation": "OperationId",
	"process":   "ProcessId",
}

func addPath(doc *openapiDoc, m *descriptorpb.MethodDescriptorProto, rule *httpRule, ix *index) {
	params := pathParams(rule.path)
	op := &operation{
		OperationID: toLowerCamel(m.GetName()),
		Summary:     m.GetName(),
		Parameters:  refsToParams(params),
	}

	if rule.body != "" {
		var body *Schema
		if rule.body == "*" {
			body = refSchema(messageSimpleName(m.GetInputType()))
		} else {
			in := ix.messages[m.GetInputType()]
			if fld := findField(in, rule.body); fld != nil {
				body = schemaForField(fld, ix)
			}
		}
		if body != nil {
			op.RequestBody = &requestBody{Required: true, Content: map[string]contentEntry{
				"application/json": {Schema: body},
			}}
		}
	}

	if rule.method == "POST" || rule.method == "PUT" || rule.method == "PATCH" || rule.method == "DELETE" {
		op.Parameters = append(op.Parameters, parameter{
			Name: "Idempotency-Key", In: "header", Required: true,
			Schema:      &Schema{Type: "string"},
			Description: "Opaque client-provided key for idempotent retries; required for all mutations.",
		})
	}

	code := rule.code
	if code == 0 {
		code = 200
	}

	// Streaming (watch) -> SSE.
	if m.GetServerStreaming() {
		op.Responses = map[string]response{
			fmt.Sprintf("%d", code): {
				Description: "Server-sent event stream of resource snapshots.",
				Content:     map[string]contentEntry{"text/event-stream": {Schema: &Schema{Type: "string"}}},
			},
			"401": {Description: "UNAUTHENTICATED"},
		}
	} else {
		resp := response{Description: "OK"}
		if out := m.GetOutputType(); out != "" && out != ".google.protobuf.Empty" {
			resp.Content = map[string]contentEntry{
				"application/json": {Schema: refSchema(messageSimpleName(out))},
			}
		}
		op.Responses = map[string]response{
			fmt.Sprintf("%d", code): resp,
			"400":                   {Description: "INVALID_ARGUMENT"},
			"401":                   {Description: "UNAUTHENTICATED"},
			"403":                   {Description: "PERMISSION_DENIED"},
			"404":                   {Description: "NOT_FOUND"},
			"409":                   {Description: "IDEMPOTENCY_KEY_REUSED / IDEMPOTENCY_IN_PROGRESS"},
			"429":                   {Description: "QUOTA_EXCEEDED / RATE_LIMITED"},
			"503":                   {Description: "CAPACITY_UNAVAILABLE / UNAVAILABLE"},
		}
	}

	pi := doc.Paths[pathTemplateToOpenAPI(rule.path)]
	switch strings.ToLower(rule.method) {
	case "get":
		pi.Get = op
	case "post":
		pi.Post = op
	case "put":
		pi.Put = op
	case "delete":
		pi.Delete = op
	}
	doc.Paths[pathTemplateToOpenAPI(rule.path)] = pi
}

func pathParams(path string) []string {
	var out []string
	start := -1
	for i, r := range path {
		if r == '{' {
			start = i + 1
		} else if r == '}' && start >= 0 {
			out = append(out, path[start:i])
			start = -1
		}
	}
	return out
}

func refsToParams(params []string) []parameter {
	out := make([]parameter, 0, len(params))
	for _, p := range params {
		out = append(out, parameter{
			Name: p, In: "path", Required: true, Schema: &Schema{Type: "string"},
		})
	}
	return out
}

func findField(m *descriptorpb.DescriptorProto, name string) *descriptorpb.FieldDescriptorProto {
	if m == nil {
		return nil
	}
	for _, f := range m.GetField() {
		if f.GetName() == name {
			return f
		}
	}
	return nil
}

func pathTemplateToOpenAPI(path string) string {
	return path // {param} tokens are valid OpenAPI path templates.
}

// --------------------------------------------------------------------------
// String helpers
// --------------------------------------------------------------------------

func toLowerCamel(s string) string {
	// Proto method names are CamelCase (e.g. CreateSandbox); lowercase the
	// first rune and preserve the rest. For snake_case inputs, build
	// lowerCamelCase by lowercasing the first word and title-casing the rest.
	if s == "" {
		return s
	}
	if !strings.Contains(s, "_") {
		r, sz := utf8.DecodeRuneInString(s)
		return string(unicode.ToLower(r)) + s[sz:]
	}
	parts := strings.Split(s, "_")
	out := strings.ToLower(parts[0])
	for _, p := range parts[1:] {
		if p == "" {
			continue
		}
		r, sz := utf8.DecodeRuneInString(p)
		out += string(unicode.ToUpper(r)) + p[sz:]
	}
	return out
}

// --------------------------------------------------------------------------
// Output
// --------------------------------------------------------------------------

// marshalOpenAPI renders the document. yaml.v3 sorts map keys and preserves
// struct field declaration order, so the output is deterministic without
// manual ordering.
func marshalOpenAPI(doc *openapiDoc) ([]byte, error) {
	return yaml.Marshal(doc)
}

func ptrString(s string) *string { return &s }

// --------------------------------------------------------------------------
// main: protoc plugin protocol
// --------------------------------------------------------------------------

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Println("protoc-gen-ignopenapi v0.1.0 (Ignition)")
		return
	}
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		fail(err)
	}
	req := &pluginpb.CodeGeneratorRequest{}
	if err := proto.Unmarshal(data, req); err != nil {
		fail(err)
	}
	resp, err := generate(req)
	if err != nil {
		// Report the error back to the caller as a plugin error.
		fmt.Fprintln(os.Stderr, "protoc-gen-ignopenapi:", err)
		os.Exit(1)
	}
	out, err := proto.Marshal(resp)
	if err != nil {
		fail(err)
	}
	if _, err := os.Stdout.Write(out); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "protoc-gen-ignopenapi:", err)
	os.Exit(1)
}
