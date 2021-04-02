package capture

import "testing"

func TestPing(t *testing.T) {
	c := &Ping{Host: "www.baidu.com"}
	c.SetEndpoint(endpoint)
	result, err := c.Run()
	if err != nil {
		t.Fatal(err)
	}
	if !result.Ok {
		t.Fatal(result)
	}
}
