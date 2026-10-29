package utils

import (
	"fmt"
	"strings"

	"github.com/dop251/goja"
)

// jsonStringifier 按 V8 的 JSON.stringify 语义输出格式化文本。
// Goja 自带的 JSON.stringify 缩进规则与 V8 不一致，会导致同一份配置与 Sub-Store 逐字节不一致。
type jsonStringifier struct {
	vm     *goja.Runtime
	native goja.Value // 原始 JSON.stringify，用于标量与紧凑格式
	indent int
}

// stringifyJSON 按 JSON.stringify(value, null, indent) 的语义序列化。
func stringifyJSON(vm *goja.Runtime, native goja.Value, value goja.Value, indent int) string {
	s := &jsonStringifier{vm: vm, native: native, indent: indent}
	return s.stringify(value, 0)
}

func (s *jsonStringifier) stringify(value goja.Value, depth int) string {
	if value == nil || goja.IsUndefined(value) {
		return "undefined"
	}
	if goja.IsNull(value) {
		return "null"
	}
	if obj, ok := value.(*goja.Object); ok {
		if obj.ClassName() == "Array" {
			return s.stringifyArray(obj, depth)
		}
		return s.stringifyObject(obj, depth)
	}
	return s.nativeCall(value)
}

// nativeCall 用原始 JSON.stringify 输出标量。
func (s *jsonStringifier) nativeCall(value goja.Value) string {
	fn, ok := goja.AssertFunction(s.native)
	if !ok {
		return "null"
	}
	result, err := fn(goja.Undefined(), value)
	if err != nil || result == nil || goja.IsUndefined(result) {
		return "null"
	}
	return result.String()
}

func (s *jsonStringifier) stringifyArray(obj *goja.Object, depth int) string {
	lengthValue := obj.Get("length")
	length := int(lengthValue.ToInteger())
	if length == 0 {
		return "[]"
	}
	var builder strings.Builder
	builder.WriteString("[")
	for i := range length {
		if i > 0 {
			builder.WriteString(",")
		}
		builder.WriteString("\n")
		builder.WriteString(s.pad(depth + 1))
		element := obj.Get(fmt.Sprintf("%d", i))
		if element == nil || goja.IsUndefined(element) {
			// 数组里的 undefined / 稀疏元素按 null 输出。
			builder.WriteString("null")
			continue
		}
		builder.WriteString(s.stringify(element, depth+1))
	}
	builder.WriteString("\n")
	builder.WriteString(s.pad(depth))
	builder.WriteString("]")
	return builder.String()
}

func (s *jsonStringifier) stringifyObject(obj *goja.Object, depth int) string {
	keys := obj.Keys()
	entries := make([]string, 0, len(keys))
	for _, key := range keys {
		value := obj.Get(key)
		if value == nil || goja.IsUndefined(value) {
			// 对象里值为 undefined 的属性会被 JSON.stringify 丢弃。
			continue
		}
		entries = append(entries, s.nativeCall(s.vm.ToValue(key))+": "+s.stringify(value, depth+1))
	}
	if len(entries) == 0 {
		return "{}"
	}
	var builder strings.Builder
	builder.WriteString("{")
	for i, entry := range entries {
		if i > 0 {
			builder.WriteString(",")
		}
		builder.WriteString("\n")
		builder.WriteString(s.pad(depth + 1))
		builder.WriteString(entry)
	}
	builder.WriteString("\n")
	builder.WriteString(s.pad(depth))
	builder.WriteString("}")
	return builder.String()
}

func (s *jsonStringifier) pad(depth int) string {
	return strings.Repeat(" ", depth*s.indent)
}
