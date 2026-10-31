package tools

import (
	"reflect"
	"testing"
)

func mustType(t *testing.T, v any) reflect.Type {
	t.Helper()
	return reflect.TypeOf(v)
}
