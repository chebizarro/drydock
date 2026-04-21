//go:build cgo

package symbols

import (
	"testing"
)

func TestExtractGo(t *testing.T) {
	src := []byte(`package main

func Hello() string {
	return "hello"
}

type Greeter struct {
	Name string
}

func (g *Greeter) Greet() string {
	return "Hello, " + g.Name
}
`)
	e := New()
	defer e.Close()

	syms, err := e.Extract("go", src)
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]SymbolKind{
		"Hello":   KindFunction,
		"Greeter": KindType,
		"Greet":   KindMethod,
	}
	if len(syms) != len(want) {
		t.Fatalf("expected %d symbols, got %d: %+v", len(want), len(syms), syms)
	}
	for _, sym := range syms {
		if wantKind, ok := want[sym.Name]; !ok {
			t.Errorf("unexpected symbol: %s", sym.Name)
		} else if sym.Kind != wantKind {
			t.Errorf("symbol %s: expected kind %s, got %s", sym.Name, wantKind, sym.Kind)
		}
	}
}

func TestExtractPython(t *testing.T) {
	src := []byte(`class Greeter:
    def __init__(self, name):
        self.name = name

    def greet(self):
        return f"Hello, {self.name}"

def hello():
    return "hello"
`)
	e := New()
	defer e.Close()

	syms, err := e.Extract("python", src)
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]SymbolKind{
		"Greeter":  KindClass,
		"__init__": KindFunction, // inside class → parent set
		"greet":    KindFunction, // inside class → parent set
		"hello":    KindFunction,
	}
	if len(syms) != len(want) {
		t.Fatalf("expected %d symbols, got %d: %+v", len(want), len(syms), syms)
	}

	// Verify parent tracking for class methods
	for _, sym := range syms {
		if sym.Name == "__init__" || sym.Name == "greet" {
			if sym.Parent != "Greeter" {
				t.Errorf("symbol %s: expected parent Greeter, got %q", sym.Name, sym.Parent)
			}
		}
	}
}

func TestExtractJavaScript(t *testing.T) {
	src := []byte(`function hello() {
    return "hello";
}

class Greeter {
    constructor(name) {
        this.name = name;
    }
    greet() {
        return "Hello, " + this.name;
    }
}
`)
	e := New()
	defer e.Close()

	syms, err := e.Extract("javascript", src)
	if err != nil {
		t.Fatal(err)
	}

	names := make(map[string]bool)
	for _, sym := range syms {
		names[sym.Name] = true
	}

	for _, want := range []string{"hello", "Greeter", "constructor", "greet"} {
		if !names[want] {
			t.Errorf("missing symbol: %s (found: %+v)", want, syms)
		}
	}
}

func TestExtractTypeScript(t *testing.T) {
	src := []byte(`function hello(): string {
    return "hello";
}

interface Drawable {
    draw(): void;
}

type Point = { x: number; y: number };

enum Color {
    Red,
    Green,
    Blue
}

class Shape {
    render() {}
}
`)
	e := New()
	defer e.Close()

	syms, err := e.Extract("typescript", src)
	if err != nil {
		t.Fatal(err)
	}

	names := make(map[string]SymbolKind)
	for _, sym := range syms {
		names[sym.Name] = sym.Kind
	}

	wantNames := []string{"hello", "Drawable", "Point", "Color", "Shape", "render"}
	for _, name := range wantNames {
		if _, ok := names[name]; !ok {
			t.Errorf("missing symbol: %s (found: %+v)", name, syms)
		}
	}

	if names["Drawable"] != KindInterface {
		t.Errorf("Drawable should be interface, got %s", names["Drawable"])
	}
	if names["Color"] != KindEnum {
		t.Errorf("Color should be enum, got %s", names["Color"])
	}
}

func TestExtractRust(t *testing.T) {
	src := []byte(`fn hello() -> String {
    String::from("hello")
}

struct Point {
    x: f64,
    y: f64,
}

enum Color {
    Red,
    Green,
}

trait Drawable {
    fn draw(&self);
}

impl Drawable for Point {
    fn draw(&self) {}
}
`)
	e := New()
	defer e.Close()

	syms, err := e.Extract("rust", src)
	if err != nil {
		t.Fatal(err)
	}

	names := make(map[string]SymbolKind)
	for _, sym := range syms {
		names[sym.Name] = sym.Kind
	}

	if names["hello"] != KindFunction {
		t.Errorf("hello should be function, got %s", names["hello"])
	}
	if names["Point"] != KindStruct {
		t.Errorf("Point should be struct, got %s", names["Point"])
	}
	if names["Color"] != KindEnum {
		t.Errorf("Color should be enum, got %s", names["Color"])
	}
	if names["Drawable"] != KindTrait {
		t.Errorf("Drawable should be trait, got %s", names["Drawable"])
	}

	// impl_item's draw method should have parent "Point"
	for _, sym := range syms {
		if sym.Name == "draw" && sym.Parent == "Point" {
			return // found
		}
	}
	// The impl_item with type=Point contains fn draw
	// Check that draw exists at all
	if _, ok := names["draw"]; !ok {
		t.Error("missing draw symbol from impl block")
	}
}

func TestExtractC(t *testing.T) {
	src := []byte(`int main(int argc, char **argv) {
    return 0;
}

struct Point {
    int x;
    int y;
};

void hello(void) {}
`)
	e := New()
	defer e.Close()

	syms, err := e.Extract("c", src)
	if err != nil {
		t.Fatal(err)
	}

	names := make(map[string]SymbolKind)
	for _, sym := range syms {
		names[sym.Name] = sym.Kind
	}

	if names["main"] != KindFunction {
		t.Errorf("main should be function, got %s", names["main"])
	}
	if names["Point"] != KindStruct {
		t.Errorf("Point should be struct, got %s", names["Point"])
	}
	if names["hello"] != KindFunction {
		t.Errorf("hello should be function, got %s", names["hello"])
	}
}

func TestExtractJava(t *testing.T) {
	src := []byte(`public class Greeter {
    private String name;

    public String greet() {
        return "Hello, " + name;
    }
}

interface Drawable {
    void draw();
}

enum Color { RED, GREEN, BLUE }
`)
	e := New()
	defer e.Close()

	syms, err := e.Extract("java", src)
	if err != nil {
		t.Fatal(err)
	}

	names := make(map[string]SymbolKind)
	for _, sym := range syms {
		names[sym.Name] = sym.Kind
	}

	if names["Greeter"] != KindClass {
		t.Errorf("Greeter should be class, got %s", names["Greeter"])
	}
	if names["greet"] != KindMethod {
		t.Errorf("greet should be method, got %s", names["greet"])
	}
	if names["Drawable"] != KindInterface {
		t.Errorf("Drawable should be interface, got %s", names["Drawable"])
	}
	if names["Color"] != KindEnum {
		t.Errorf("Color should be enum, got %s", names["Color"])
	}
}

func TestExtractRuby(t *testing.T) {
	src := []byte(`class Greeter
  def initialize(name)
    @name = name
  end

  def greet
    "Hello, #{@name}"
  end
end

module Utils
  def self.helper
    42
  end
end
`)
	e := New()
	defer e.Close()

	syms, err := e.Extract("ruby", src)
	if err != nil {
		t.Fatal(err)
	}

	names := make(map[string]SymbolKind)
	for _, sym := range syms {
		names[sym.Name] = sym.Kind
	}

	if names["Greeter"] != KindClass {
		t.Errorf("Greeter should be class, got %s", names["Greeter"])
	}
	if _, ok := names["initialize"]; !ok {
		t.Error("missing initialize method")
	}
	if names["Utils"] != KindModule {
		t.Errorf("Utils should be module, got %s", names["Utils"])
	}
}

func TestExtractChanged(t *testing.T) {
	src := []byte(`package main

func Hello() string {
	return "hello"
}

func Goodbye() string {
	return "goodbye"
}
`)
	e := New()
	defer e.Close()

	// Only lines 2-4 changed (0-based) — Hello function
	syms, err := e.ExtractChanged("go", src, []uint32{2, 3, 4})
	if err != nil {
		t.Fatal(err)
	}

	if len(syms) != 1 {
		t.Fatalf("expected 1 changed symbol, got %d: %+v", len(syms), syms)
	}
	if syms[0].Name != "Hello" {
		t.Errorf("expected Hello, got %s", syms[0].Name)
	}
}

func TestExtractChangedEmpty(t *testing.T) {
	src := []byte(`package main

func Hello() string {
	return "hello"
}
`)
	e := New()
	defer e.Close()

	// No changed lines → return all
	syms, err := e.ExtractChanged("go", src, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(syms) != 1 {
		t.Fatalf("expected 1 symbol, got %d", len(syms))
	}
}

func TestSupportedLanguage(t *testing.T) {
	for _, lang := range []string{"go", "python", "javascript", "typescript", "rust", "c", "cpp", "java", "ruby"} {
		if !SupportedLanguage(lang) {
			t.Errorf("%s should be supported", lang)
		}
	}
	if SupportedLanguage("brainfuck") {
		t.Error("brainfuck should not be supported")
	}
}


func TestUnsupportedLanguage(t *testing.T) {
	e := New()
	defer e.Close()

	_, err := e.Extract("brainfuck", []byte("hello"))
	if err == nil {
		t.Error("expected error for unsupported language")
	}
}
