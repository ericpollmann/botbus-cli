package daemon

import "encoding/json"

type e2eContent struct {
	S string `json:"s"`
	B string `json:"b"`
}

func encodeContent(subject, body string) []byte {
	b, _ := json.Marshal(e2eContent{S: subject, B: body})
	return b
}

func decodeContent(b []byte) (subject, body string, err error) {
	var c e2eContent
	if err = json.Unmarshal(b, &c); err != nil {
		return "", "", err
	}
	return c.S, c.B, nil
}
