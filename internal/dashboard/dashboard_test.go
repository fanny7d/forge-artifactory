package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandlerServesDashboardAndAssets(t *testing.T) {
	handler := Handler()

	for _, test := range []struct {
		path        string
		contentType string
		contains    string
	}{
		{path: "/dashboard/", contentType: "text/html", contains: "Forge Artifactory"},
		{path: "/dashboard/styles.css", contentType: "text/css", contains: ":root"},
		{path: "/dashboard/app.js", contentType: "javascript", contains: "function api"},
		{path: "/dashboard/repositories/example", contentType: "text/html", contains: "Forge Artifactory"},
	} {
		t.Run(test.path, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, test.path, nil)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d", response.Code)
			}
			if contentType := response.Header().Get("Content-Type"); !strings.Contains(contentType, test.contentType) {
				t.Fatalf("content type = %q", contentType)
			}
			if !strings.Contains(response.Body.String(), test.contains) {
				t.Fatalf("response does not contain %q", test.contains)
			}
		})
	}
}

func TestDialogCancellationCannotSubmitBusinessForm(t *testing.T) {
	index, err := embedded.ReadFile("assets/index.html")
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	html := string(index)
	for _, required := range []string{
		`id="dialog-close" class="icon-button" type="button"`,
		`id="dialog-cancel" class="button subtle" type="button"`,
		`id="dialog-submit" class="button primary" type="submit"`,
	} {
		if !strings.Contains(html, required) {
			t.Errorf("index does not contain %q", required)
		}
	}
	for _, forbidden := range []string{`formmethod="dialog"`, `value="cancel"`} {
		if strings.Contains(html, forbidden) {
			t.Errorf("index contains submitting cancel behavior %q", forbidden)
		}
	}

	app, err := embedded.ReadFile("assets/app.js")
	if err != nil {
		t.Fatalf("read app: %v", err)
	}
	javascript := string(app)
	for _, required := range []string{
		`elements.dialogClose.addEventListener("click", () => closeDialog())`,
		`elements.dialogCancel.addEventListener("click", () => closeDialog())`,
		`elements.dialog.addEventListener("cancel"`,
		`event.key === "Escape" && elements.dialog.open`,
	} {
		if !strings.Contains(javascript, required) {
			t.Errorf("app does not contain %q", required)
		}
	}
	if strings.Contains(javascript, `addEventListener("click", closeDialog)`) {
		t.Error("click event object must not be passed as closeDialog's force argument")
	}
}
