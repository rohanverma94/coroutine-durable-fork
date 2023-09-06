package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/format"
	"go/types"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"

	"github.com/stealthrocket/coroutine/serde"
	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/types/typeutil"
)

func usage() {
	fmt.Fprintf(os.Stderr, "Usage of serde:\n")
	fmt.Fprintf(os.Stderr, "\tserde [flags] -type T [directory]\n")
	fmt.Fprintf(os.Stderr, "\tserde [flags] -type T files...\n")
	fmt.Fprintf(os.Stderr, "Flags:\n")
	flag.PrintDefaults()
}

func main() {
	typeName := ""
	flag.StringVar(&typeName, "type", "", "non-optional type name")
	output := ""
	flag.StringVar(&output, "output", "", "output file name; defaults to <type_serde.go")
	flag.Usage = usage
	flag.Parse()

	if len(typeName) == 0 {
		fmt.Fprintf(os.Stderr, "missing type name (-type is required)\n")
		flag.Usage()
		os.Exit(2)
	}

	args := flag.Args()
	if len(args) == 0 {
		args = []string{"."}
	}

	err := generate(typeName, args, output)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		os.Exit(1)
	}
}

func generate(typeName string, patterns []string, output string) error {
	pkgs, err := parse(patterns)
	if err != nil {
		return err
	}

	// Find the package that contains the type declaration requested.
	// This will also be the output package.
	td := findTypeDef(typeName, pkgs)
	if td == notype {
		return fmt.Errorf("could not find type definition")
	}

	output = td.TargetFile()

	g := generator{
		output: td.TargetFile(),
		main:   td.pkg,
	}
	//	fmt.Println("OUTPUT:")

	g.Typedef(td)

	var buf bytes.Buffer
	n, err := g.WriteTo(&buf)
	if err != nil {
		panic(fmt.Errorf("couldn't write (%d bytes): %w", n, err))
	}

	clean, err := format.Source(buf.Bytes())
	if err != nil {
		fmt.Println(buf.String())
		return err
	}
	//	fmt.Println(string(clean))

	f, err := os.OpenFile(output, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("opening '%s': %w", output, err)
	}
	defer f.Close()

	_, err = f.Write(clean)

	fmt.Println("[GEN]", output)

	return err
}

type location struct {
	//pkg    *types.Package
	pkg    string
	name   string
	method bool
}

type locations struct {
	serializer   location
	deserializer location
}

type generator struct {
	// Map[types.Type] -> locations to track the types that already have
	// their serialization functions emitted.
	known typeutil.Map
	// Map a package name to its import path.
	imports map[string]string

	// Path where the code should be written.
	output string
	// Package the output file belongs to.
	main *packages.Package
	// Output.
	s *strings.Builder
}

func (g *generator) W(f string, args ...any) {
	if g.s == nil {
		g.s = &strings.Builder{}
	}
	fmt.Fprintf(g.s, f, args...)
	g.s.WriteString("\n")
}

// Generate the code for a given typedef
func (g *generator) Typedef(t typedef) {
	typeName := g.TypeNameFor(t.obj.Type())
	g.Type(t.obj.Type(), typeName)
}

func (g *generator) WriteTo(w io.Writer) (int64, error) {
	n, err := fmt.Fprintf(w, "// Code generated by coroc. DO NOT EDIT.\n\npackage %s\n", g.main.Name)
	if err != nil {
		return int64(n), err
	}
	for name, path := range g.imports {
		n2, err := fmt.Fprintf(w, "import %s \"%s\"\n", name, path)
		n += n2
		if err != nil {
			return int64(n), err
		}
	}

	n2, err := w.Write([]byte(g.s.String()))
	return int64(n) + int64(n2), err
}

func (g *generator) Type(t types.Type, name string) locations {
	switch x := t.(type) {
	case *types.Basic:
		return g.Basic(x, name)
	case *types.Struct:
		return g.Struct(x, name)
	case *types.Named:
		return g.Named(x, name)
	case *types.Slice:
		return g.Slice(x, name)
	default:
		panic(fmt.Errorf("type generator not implemented: %s (%T)", t, t))
	}
}

func (g *generator) Slice(t *types.Slice, name string) locations {
	if loc, ok := g.get(t); ok {
		return loc
	}

	loc := g.newGenLocation(t, name)

	et := t.Elem()
	typeName := g.TypeNameFor(et)
	eloc := g.Type(et, typeName)

	g.W(`func %s(x %s, b []byte) []byte {`, loc.serializer.name, name)
	g.W(`b = serde.SerializeSliceSize(x, b)`)
	g.W(`for _, x := range x {`)
	g.serializeCallForLoc(eloc)
	g.W(`}`)
	g.W(`return b`)
	g.W(`}`)
	g.W(``)

	g.W(`func %s(b []byte) (%s, []byte) {`, loc.deserializer.name, name)
	g.W(`n, b := serde.DeserializeSliceSize(b)`)
	g.W(`var z %s`, name)
	g.W(`for i := 0; i < n; i++ {`)
	g.W(`var x %s`, typeName)
	g.deserializeCallForLoc(eloc)
	g.W(`z = append(z, x)`)
	g.W(`}`)
	g.W(`return z, b`)
	g.W(`}`)
	g.W(``)

	return loc
}

func (g *generator) Named(t *types.Named, name string) locations {
	typeName := g.TypeNameFor(t.Obj().Type())
	return g.Type(t.Underlying(), typeName)
}

func (g *generator) Struct(t *types.Struct, name string) locations {
	if loc, ok := g.get(t); ok {
		return loc
	}

	loc := g.newGenLocation(t, name)

	n := t.NumFields()
	for i := 0; i < n; i++ {
		f := t.Field(i)
		ft := f.Type()
		typeName := g.TypeNameFor(ft)
		g.Type(ft, typeName)
	}

	// Generate a new function to serialize this struct type.
	g.W(`func %s(x %s, b []byte) []byte {`, loc.serializer.name, name)
	// TODO: private fields
	for i := 0; i < n; i++ {
		f := t.Field(i)
		ft := f.Type()

		typeName := g.TypeNameFor(ft)
		floc := g.Type(ft, typeName)

		g.W(`{`)
		g.W(`x := x.%s`, f.Name())
		g.serializeCallForLoc(floc)
		g.W(`}`)
	}
	g.W(`return b`)
	g.W(`}`)
	g.W(``)

	g.W(`func %s(b []byte) (%s, []byte) {`, loc.deserializer.name, name)
	g.W(`var z %s`, name)
	// TODO: private fields
	for i := 0; i < n; i++ {
		f := t.Field(i)
		ft := f.Type()

		typeName := g.TypeNameFor(ft)
		floc := g.Type(ft, typeName)

		g.W(`{`)
		g.W(`var x %s`, typeName)
		g.deserializeCallForLoc(floc)
		g.W(`z.%s = x`, f.Name())
		g.W(`}`)
	}
	g.W(`return z, b`)
	g.W(`}`)
	g.W(``)

	return loc
}

func (g *generator) serializeCallForLoc(loc locations) {
	l := loc.serializer
	if l.method && l.pkg != "" {
		panic("cannot have both a package prefix and be a method")
	}
	if l.method {
		g.W(`b = x.%s(b)`, l.name)
	} else if l.pkg != "" {
		g.W(`b = %s.%s(x, b)`, l.pkg, l.name)
	} else {
		g.W(`b = %s(x, b)`, l.name)
	}
}

func (g *generator) deserializeCallForLoc(loc locations) {
	l := loc.deserializer
	if l.method && l.pkg != "" {
		panic("cannot have both a package prefix and be a method")
	}
	if l.method {
		g.W(`b = x.%s(b)`, l.name)
	} else if l.pkg != "" {
		g.W(`x, b = %s.%s(b)`, l.pkg, l.name)
	} else {
		g.W(`x, b = %s(b)`, l.name)
	}
}

func isInvalidChar(r rune) bool {
	valid := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
	return !valid
}

// Generate, save, and return a new location for a type with generated
// serializers.
func (g *generator) newGenLocation(t types.Type, name string) locations {
	//TODO: check name collision
	if strings.ContainsFunc(name, isInvalidChar) {
		name = ""
	}
	if name == "" {
		name = fmt.Sprintf("gen%d", g.known.Len())
	}
	loc := locations{
		serializer: location{
			name: "Serialize_" + name,
		},
		deserializer: location{
			name: "Deserialize_" + name,
		},
	}
	prev := g.known.Set(t, loc)
	if prev != nil {
		panic(fmt.Errorf("trying to override known location"))
	}
	return loc
}

func (g *generator) Basic(t *types.Basic, name string) locations {
	g.ensureImport("serde", "github.com/stealthrocket/coroutine/serde")
	nameof := func(x interface{}) string {
		full := runtime.FuncForPC(reflect.ValueOf(x).Pointer()).Name()
		return full[strings.LastIndexByte(full, '.')+1:]
	}
	l := locations{
		serializer:   location{pkg: "serde", name: ""},
		deserializer: location{pkg: "serde", name: ""},
	}

	switch t.Kind() {
	case types.Invalid:
		panic("trying to generate serializer for invalid basic type")
	case types.String:
		l.serializer.name = nameof(serde.SerializeString)
		l.deserializer.name = nameof(serde.DeserializeString)
	case types.Bool:
		l.serializer.name = nameof(serde.SerializeBool)
		l.deserializer.name = nameof(serde.DeserializeBool)
	case types.Int64:
		l.serializer.name = nameof(serde.SerializeInt64)
		l.deserializer.name = nameof(serde.DeserializeInt64)
	case types.Int32:
		l.serializer.name = nameof(serde.SerializeInt32)
		l.deserializer.name = nameof(serde.DeserializeInt32)
	case types.Int16:
		l.serializer.name = nameof(serde.SerializeInt16)
		l.deserializer.name = nameof(serde.DeserializeInt16)
	case types.Int8:
		l.serializer.name = nameof(serde.SerializeInt8)
		l.deserializer.name = nameof(serde.DeserializeInt8)
	case types.Uint64:
		l.serializer.name = nameof(serde.SerializeUint64)
		l.deserializer.name = nameof(serde.DeserializeUint64)
	case types.Uint32:
		l.serializer.name = nameof(serde.SerializeUint32)
		l.deserializer.name = nameof(serde.DeserializeUint32)
	case types.Uint16:
		l.serializer.name = nameof(serde.SerializeUint16)
		l.deserializer.name = nameof(serde.DeserializeUint16)
	case types.Uint8:
		l.serializer.name = nameof(serde.SerializeUint8)
		l.deserializer.name = nameof(serde.DeserializeUint8)
	case types.Float32:
		l.serializer.name = nameof(serde.SerializeFloat32)
		l.deserializer.name = nameof(serde.DeserializeFloat32)
	case types.Float64:
		l.serializer.name = nameof(serde.SerializeFloat64)
		l.deserializer.name = nameof(serde.DeserializeFloat64)
	case types.Complex64:
		l.serializer.name = nameof(serde.SerializeComplex64)
		l.deserializer.name = nameof(serde.DeserializeComplex64)
	case types.Complex128:
		l.serializer.name = nameof(serde.SerializeComplex128)
		l.deserializer.name = nameof(serde.DeserializeComplex128)
	default:
		panic(fmt.Errorf("basic type kind %s not handled", basicKindString(t)))
	}
	return l
}

func (g *generator) TypeNameFor(t types.Type) string {
	return types.TypeString(t, types.RelativeTo(g.main.Types))
}

func (g *generator) get(t types.Type) (locations, bool) {
	loc := g.known.At(t)
	if loc == nil {
		return locations{}, false
	}
	return loc.(locations), true
}

func (g *generator) ensureImport(name, path string) {
	if g.imports == nil {
		g.imports = make(map[string]string)
	}
	p, ok := g.imports[name]
	if ok && p != path {
		panic(fmt.Errorf("two imports named '%s': '%s' and '%s'", name, path, p))
	}
	if !ok {
		g.imports[name] = path
	}
}

type typedef struct {
	obj types.Object
	pkg *packages.Package
}

// TargetFile returns the path where a serder function should be generated for
// this type.
func (t typedef) TargetFile() string {
	pos := t.pkg.Fset.Position(t.obj.Pos())
	dir, file := filepath.Split(pos.Filename)

	i := strings.LastIndexByte(file, '.')
	if i == -1 {
		panic(fmt.Errorf("files does not end in .go: %s", file))
	}
	outFile := file[:i] + "_serde.go"
	return filepath.Join(dir, outFile)
}

var notype = typedef{}

func findTypeDef(name string, pkgs []*packages.Package) typedef {
	for _, pkg := range pkgs {
		for id, d := range pkg.TypesInfo.Defs {
			if id.Name == name {
				// TOOD: this probably need more checks.
				return typedef{obj: d, pkg: pkg}
			}
		}
	}
	return notype
}

func parse(patterns []string) ([]*packages.Package, error) {
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedTypes | packages.NeedTypesInfo | packages.NeedSyntax | packages.NeedDeps | packages.NeedImports,
	}
	return packages.Load(cfg, patterns...)
}

func basicKindString(x *types.Basic) string {
	return [...]string{
		types.Invalid:       "Invalid",
		types.Bool:          "Bool",
		types.Int:           "Int",
		types.Int8:          "Int8",
		types.Int16:         "Int16",
		types.Int32:         "Int32",
		types.Int64:         "Int64",
		types.Uint:          "Uint",
		types.Uint8:         "Uint8",
		types.Uint16:        "Uint16",
		types.Uint32:        "Uint32",
		types.Uint64:        "Uint64",
		types.Uintptr:       "Uintptr",
		types.Float32:       "Float32",
		types.Float64:       "Float64",
		types.Complex64:     "Complex64",
		types.Complex128:    "Complex128",
		types.String:        "String",
		types.UnsafePointer: "UnsafePointer",

		types.UntypedBool:    "UntypedBool",
		types.UntypedInt:     "UntypedInt",
		types.UntypedRune:    "UntypedRune",
		types.UntypedFloat:   "UntypedFloat",
		types.UntypedComplex: "UntypedComplex",
		types.UntypedString:  "UntypedString",
		types.UntypedNil:     "UntypedNil",
	}[x.Kind()]
}
