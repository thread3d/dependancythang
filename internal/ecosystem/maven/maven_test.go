package maven_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thread3d/dependancythang/internal/ecosystem"
	"github.com/thread3d/dependancythang/internal/ecosystem/maven"
)

func seed(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	return dir
}

// fakeRunner serves canned `mvn dependency:list` output.
type fakeRunner struct {
	output   string
	commands []string
}

func (f *fakeRunner) Run(_ context.Context, _ string, name string, args ...string) (string, error) {
	f.commands = append(f.commands, name+" "+strings.Join(args, " "))
	return f.output, nil
}

const rootPom = `<?xml version="1.0"?>
<project>
  <groupId>dev.example</groupId>
  <artifactId>parent</artifactId>
  <version>1.0.0</version>
  <modules>
    <module>core</module>
    <module>cli</module>
  </modules>
</project>
`

const childPom = `<project>
  <parent><groupId>dev.example</groupId><artifactId>parent</artifactId></parent>
  <artifactId>core</artifactId>
</project>
`

// TestDetectCollapsesAReactor: modules are not separate targets, because Maven
// resolves the reactor as one graph and per-module targets would plan conflicting
// bumps of the same dependency.
func TestDetectCollapsesAReactor(t *testing.T) {
	dir := seed(t, map[string]string{
		"pom.xml":      rootPom,
		"core/pom.xml": childPom,
		"cli/pom.xml":  childPom,
	})
	targets, err := maven.New().Detect(context.Background(), dir)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if len(targets) != 1 {
		t.Fatalf("targets = %d (%+v), want 1 reactor root", len(targets), targets)
	}
	if targets[0].Dir != "/" {
		t.Errorf("target dir = %q, want the reactor root", targets[0].Dir)
	}
}

func TestDetectKeepsStandalonePoms(t *testing.T) {
	dir := seed(t, map[string]string{
		"a/pom.xml": childPom,
		"b/pom.xml": childPom,
	})
	targets, err := maven.New().Detect(context.Background(), dir)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if len(targets) != 2 {
		t.Fatalf("targets = %d, want 2 standalone poms", len(targets))
	}
}

// mvnOutput mirrors the real dependency:list format, including a trailing build
// summary line that must not be mistaken for a dependency.
const mvnOutput = `[INFO] Scanning for projects...
[INFO]
[INFO] --- maven-dependency-plugin:3.6.1:list (default-cli) @ core ---
[INFO]    dev.example:core:jar:1.0.0:compile
[INFO]    com.fasterxml.jackson.core:jackson-databind:jar:2.15.2:compile
[INFO]    org.xerial:sqlite-jdbc:jar:3.43.0.0:runtime
[INFO]    org.junit.jupiter:junit-jupiter:jar:5.9.0:test
[INFO]
[INFO] BUILD SUCCESS
`

func TestInventoryDelegatesToMaven(t *testing.T) {
	dir := seed(t, map[string]string{"pom.xml": rootPom})
	runner := &fakeRunner{output: mvnOutput}
	eco := maven.NewWithRunner(runner)

	targets, err := eco.Detect(context.Background(), dir)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	deps, err := eco.Inventory(context.Background(), targets[0])
	if err != nil {
		t.Fatalf("Inventory: %v", err)
	}

	got := map[string]ecosystem.Dependency{}
	for _, d := range deps {
		got[d.Name] = d
	}
	if _, ok := got["dev.example:core"]; ok {
		t.Error("the reactor's own module was inventoried as a third-party dependency")
	}
	if len(got) != 3 {
		t.Fatalf("deps = %d (%v), want 3", len(got), got)
	}
	if d := got["com.fasterxml.jackson.core:jackson-databind"]; d.Current != "2.15.2" || d.Dev {
		t.Errorf("jackson = %+v, want 2.15.2 and not dev", d)
	}
	if d := got["org.junit.jupiter:junit-jupiter"]; !d.Dev {
		t.Errorf("junit = %+v, want it marked as a test/dev dependency", d)
	}
	if len(runner.commands) != 1 || !strings.Contains(runner.commands[0], "dependency:list") {
		t.Errorf("commands = %v, want one dependency:list", runner.commands)
	}
}

func TestCompareOrdersMavenVersions(t *testing.T) {
	eco := maven.New()
	cases := []struct {
		a, b string
		want int
	}{
		{"5.9.0", "5.10.0", -1}, // numeric, not lexicographic
		{"2.15.2", "2.15.10", -1},
		{"1.0.0", "1.0.0", 0},
		{"1.0.0-rc1", "1.0.0", -1}, // a release outranks its own rc
		{"1.0.0-SNAPSHOT", "1.0.0", -1},
		{"1.0.0", "1.0.1", -1},
	}
	for _, tc := range cases {
		if got := eco.Compare(tc.a, tc.b); got != tc.want {
			t.Errorf("Compare(%q,%q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestClassify(t *testing.T) {
	eco := maven.New()
	cases := []struct {
		from, to string
		want     ecosystem.UpdateClass
	}{
		{"5.9.0", "5.9.1", ecosystem.ClassPatch},
		{"5.9.0", "5.10.0", ecosystem.ClassMinor},
		{"5.9.0", "6.0.0", ecosystem.ClassMajor},
		{"5.9.0", "5.9.0", ecosystem.ClassNone},
		{"1.0.0-SNAPSHOT", "1.0.0", ecosystem.ClassPatch},
		{"RELEASE", "1.2.3", ecosystem.ClassPin},
	}
	for _, tc := range cases {
		if got := eco.Classify(tc.from, tc.to); got != tc.want {
			t.Errorf("Classify(%q,%q) = %q, want %q", tc.from, tc.to, got, tc.want)
		}
	}
}

const pomWithDeps = `<project>
  <groupId>dev.example</groupId>
  <artifactId>app</artifactId>
  <version>1.0.0</version>
  <properties>
    <jackson.version>2.15.2</jackson.version>
  </properties>
  <dependencies>
    <dependency>
      <groupId>org.xerial</groupId>
      <artifactId>sqlite-jdbc</artifactId>
      <version>3.43.0.0</version>
    </dependency>
    <dependency>
      <groupId>com.fasterxml.jackson.core</groupId>
      <artifactId>jackson-databind</artifactId>
      <version>${jackson.version}</version>
    </dependency>
    <dependency>
      <groupId>org.junit.jupiter</groupId>
      <artifactId>junit-jupiter</artifactId>
    </dependency>
  </dependencies>
</project>
`

// TestApplyRewritesOnlyLiteralVersions is the honest limit of the write path: a
// property-indirected version and a centrally managed one are both refused, because
// editing the element would create a second source of truth.
func TestApplyRewritesOnlyLiteralVersions(t *testing.T) {
	dir := seed(t, map[string]string{"pom.xml": pomWithDeps})
	res, err := maven.New().Apply(context.Background(), dir, []ecosystem.Change{
		{Name: "org.xerial:sqlite-jdbc", From: "3.43.0.0", To: "3.44.0.0"},
		{Name: "com.fasterxml.jackson.core:jackson-databind", From: "2.15.2", To: "2.16.0"},
		{Name: "org.junit.jupiter:junit-jupiter", From: "5.9.0", To: "5.10.0"},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "pom.xml"))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	text := string(got)

	if !strings.Contains(text, "<version>3.44.0.0</version>") {
		t.Errorf("the literal version was not rewritten:\n%s", text)
	}
	if strings.Contains(text, "<version>2.16.0</version>") {
		t.Error("a property-indirected version was spliced into the element")
	}
	if !strings.Contains(text, "<jackson.version>2.15.2</jackson.version>") {
		t.Error("the property block was touched")
	}
	if strings.Contains(text, "junit-jupiter</artifactId>\n      <version>5.10.0") {
		t.Error("a centrally managed dependency gained a version element")
	}
	if len(res.Changed) != 1 || res.Changed[0] != "org.xerial:sqlite-jdbc" {
		t.Errorf("changed = %v, want only sqlite-jdbc", res.Changed)
	}
}

func TestVerifyCommandsUseMaven(t *testing.T) {
	cmds := maven.New().VerifyCommands(ecosystem.Target{})
	if len(cmds) == 0 || cmds[0][0] != "mvn" {
		t.Fatalf("commands = %v, want mvn", cmds)
	}
	if !contains(cmds[0], "--batch-mode") {
		t.Error("verification should run Maven non-interactively")
	}
}

func contains(argv []string, want string) bool {
	for _, a := range argv {
		if a == want {
			return true
		}
	}
	return false
}
