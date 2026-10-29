package utils

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dop251/goja"
)

// IsSingboxTemplateScript 判断脚本是否为 Sub-Store Script Operator 约定的 sing-box 渲染脚本。
// 这类脚本自带 Runtime / ProxySource / Pipeline，并通过 $arguments、$files、produceArtifact 与宿主交互。
func IsSingboxTemplateScript(scriptContent string) bool {
	content := scriptContent
	if !strings.Contains(content, "produceArtifact") {
		return false
	}
	return strings.Contains(content, "function main(env)") || strings.Contains(content, "Pipeline.run")
}

// RunSingboxScript 按 Sub-Store Script Operator 约定执行 sing-box 渲染脚本。
//
// scriptContent: 脚本源码（自带 Runtime / ProxySource / Pipeline 实现）
// templateContent: 模板文件内容，对应 Sub-Store 的 $files[0]
// argsJSON: 脚本参数，对应 $arguments
// proxiesJSON: 节点出站数组（sing-box outbound 对象），由 produceArtifact 返回
//
// 返回最终 sing-box 配置文本，与 Sub-Store 使用同一脚本时逐字节一致。
func RunSingboxScript(scriptContent, templateContent, argsJSON, proxiesJSON string) (string, error) {
	vm := goja.New()

	_ = vm.Set("console", map[string]any{
		"log":   fmt.Println,
		"info":  fmt.Println,
		"warn":  fmt.Println,
		"error": fmt.Println,
	})

	if _, err := vm.RunString(polyfills); err != nil {
		return "", fmt.Errorf("polyfill injection error: %w", err)
	}

	// Goja 的 JSON.stringify 缩进与 V8 不同，这里替换成与 V8 一致的实现，保证输出与 Sub-Store 逐字节一致。
	if jsonValue := vm.Get("JSON"); jsonValue != nil {
		if jsonObj := jsonValue.ToObject(vm); jsonObj != nil {
			nativeStringify := jsonObj.Get("stringify")
			jsonObj.Set("stringify", func(call goja.FunctionCall) goja.Value {
				indent := 0
				if len(call.Arguments) > 2 {
					indent = int(call.Argument(2).ToInteger())
				}
				if indent <= 0 {
					if fn, ok := goja.AssertFunction(nativeStringify); ok {
						result, err := fn(goja.Undefined(), call.Argument(0))
						if err == nil && result != nil {
							return result
						}
					}
					return goja.Undefined()
				}
				return vm.ToValue(stringifyJSON(vm, nativeStringify, call.Argument(0), indent))
			})
		}
	}

	wrapper := "(async function($arguments, $files, produceArtifact, $content) {\n$arguments = JSON.parse(__sublinkArgsJSON);\n" + scriptContent + "\n__sublinkResult = $content;\nreturn $content;\n})"
	fnValue, err := vm.RunString(wrapper)
	if err != nil {
		return "", fmt.Errorf("script compilation error: %w", err)
	}
	fn, ok := goja.AssertFunction(fnValue)
	if !ok {
		return "", fmt.Errorf("script wrapper is not a function")
	}

	if strings.TrimSpace(argsJSON) == "" {
		argsJSON = "{}"
	}
	if strings.TrimSpace(proxiesJSON) == "" {
		proxiesJSON = "[]"
	}
	if !json.Valid([]byte(argsJSON)) {
		return "", fmt.Errorf("script arguments are not valid JSON")
	}
	if !json.Valid([]byte(proxiesJSON)) {
		return "", fmt.Errorf("proxy payload is not valid JSON")
	}

	// 参数与节点都用 JSON 文本交给脚本在 JS 侧解析，避免 Go map 转换打乱对象键顺序。
	_ = vm.Set("__sublinkArgsJSON", argsJSON)
	_ = vm.Set("__sublinkProxiesJSON", proxiesJSON)

	files := vm.NewArray()
	if err := files.Set("0", vm.ToValue(templateContent)); err != nil {
		return "", fmt.Errorf("cannot bind template file: %w", err)
	}

	producerValue, err := vm.RunString("(async function(){ return JSON.parse(__sublinkProxiesJSON); })")
	if err != nil {
		return "", fmt.Errorf("cannot bind proxy producer: %w", err)
	}

	if _, err := fn(goja.Undefined(), vm.ToValue(""), files, producerValue, vm.ToValue("")); err != nil {
		return "", fmt.Errorf("script execution error: %w", err)
	}

	// 脚本是异步的，await 之后的收尾工作依赖微任务队列，这里驱动队列直到拿到结果。
	for range 100 {
		if _, err := vm.RunString("0"); err != nil {
			return "", fmt.Errorf("script drain error: %w", err)
		}
		value := vm.Get("__sublinkResult")
		if value != nil && !goja.IsUndefined(value) && !goja.IsNull(value) {
			return value.String(), nil
		}
	}

	return "", fmt.Errorf("script did not produce any content")
}
