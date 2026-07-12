package api

import (
	"encoding/json"
	"net/http"
)

func writeProblem(w http.ResponseWriter, problem Problem) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(problem.Status)
	_ = json.NewEncoder(w).Encode(problem)
}

func writeRequestProblem(w http.ResponseWriter, r *http.Request, problem Problem) {
	if problem.RequestId == "" {
		problem.RequestId = requestIDFromContext(r.Context())
	}
	writeProblem(w, problem)
}
