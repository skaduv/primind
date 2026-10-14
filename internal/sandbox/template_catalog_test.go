package sandbox

import "testing"

func TestIsStandardTemplateRecognizesProviderScopedName(t *testing.T) {
	for _, name := range []string{"primind", "team/primind", "project-b89e/Primind"} {
		if !isStandardTemplate(name) {
			t.Fatalf("expected %q to identify the Primind standard template", name)
		}
	}
	if isStandardTemplate("primind-custom") {
		t.Fatal("custom template must not be treated as the standard template")
	}
	if isStandardTemplate(DesktopTemplateName) {
		t.Fatal("the desktop sibling must not be classified as the CLI standard template")
	}
}

func TestIsDesktopTemplateRecognizesProviderScopedName(t *testing.T) {
	for _, name := range []string{"primind-desktop", "team/primind-desktop", "project-b89e/Primind-Desktop"} {
		if !isDesktopTemplate(name) {
			t.Fatalf("expected %q to identify the Primind desktop template", name)
		}
	}
	if isDesktopTemplate("primind") {
		t.Fatal("the CLI template must not be classified as desktop")
	}
	if isDesktopTemplate("primind-desktop-custom") {
		t.Fatal("a similarly prefixed custom name must not be the desktop template")
	}
}

func TestClassifyPrimindTemplatePrefersNameOverImage(t *testing.T) {
	standard, desktop := classifyPrimindTemplate(DesktopTemplateName, DefaultDockerImage)
	if standard || !desktop {
		t.Fatalf(
			"named desktop template must be desktop even if the image repo matches CLI, got standard=%v desktop=%v",
			standard, desktop,
		)
	}
	standard, desktop = classifyPrimindTemplate(StandardTemplateName, DefaultDesktopDockerImage)
	if !standard || desktop {
		t.Fatalf(
			"named CLI template must stay CLI even if the image tag is desktop, got standard=%v desktop=%v",
			standard, desktop,
		)
	}
}

func TestClassifyPrimindTemplateNamelessImageUsesTag(t *testing.T) {
	standard, desktop := classifyPrimindTemplate("", DefaultDockerImage)
	if !standard || desktop {
		t.Fatalf("CLI image with no name must be standard, got standard=%v desktop=%v", standard, desktop)
	}
	standard, desktop = classifyPrimindTemplate("", DefaultDesktopDockerImage)
	if standard || !desktop {
		t.Fatalf("desktop image with no name must be desktop, got standard=%v desktop=%v", standard, desktop)
	}
	standard, desktop = classifyPrimindTemplate("", DefaultCubeDesktopTemplateImage)
	if standard || !desktop {
		t.Fatalf("Cube desktop image with no name must be desktop, got standard=%v desktop=%v", standard, desktop)
	}
}
