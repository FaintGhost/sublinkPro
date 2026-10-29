package protocol

import (
	"bytes"
	"encoding/json"
)

// jsonObject 是保持插入顺序的 JSON 对象。
//
// sing-box 出站需要与 Sub-Store 的转换结果逐字节一致，而 encoding/json 对 map 会按 key 排序，
// 因此这里用有序对象按插入顺序序列化。
type jsonObject struct {
	keys   []string
	values map[string]any
}

// newJSONObject 创建空的有序对象。
func newJSONObject() *jsonObject {
	return &jsonObject{values: map[string]any{}}
}

// Set 写入字段并保持插入顺序，返回自身便于链式调用。
func (o *jsonObject) Set(key string, value any) *jsonObject {
	if _, exists := o.values[key]; !exists {
		o.keys = append(o.keys, key)
	}
	o.values[key] = value
	return o
}

// Get 读取字段。
func (o *jsonObject) Get(key string) (any, bool) {
	value, ok := o.values[key]
	return value, ok
}

// Keys 返回按插入顺序排列的字段名。
func (o *jsonObject) Keys() []string {
	return o.keys
}

// MarshalJSON 按插入顺序输出 JSON 对象。
func (o *jsonObject) MarshalJSON() ([]byte, error) {
	var buffer bytes.Buffer
	buffer.WriteByte('{')
	for index, key := range o.keys {
		if index > 0 {
			buffer.WriteByte(',')
		}
		encodedKey, err := json.Marshal(key)
		if err != nil {
			return nil, err
		}
		buffer.Write(encodedKey)
		buffer.WriteByte(':')
		encodedValue, err := json.Marshal(o.values[key])
		if err != nil {
			return nil, err
		}
		buffer.Write(encodedValue)
	}
	buffer.WriteByte('}')
	return buffer.Bytes(), nil
}
