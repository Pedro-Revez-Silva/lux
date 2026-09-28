package main

import (
	"os"
	"reflect"
	"testing"
)

// The example in the docs is a valid configuration.
func TestExampleConfig(t *testing.T) {
	if _, err := os.Stat("../../docs/luxd.example.toml"); err != nil {
		t.Fatal(err)
	}
	c, err := loadConfig("../../docs/luxd.example.toml")
	if err != nil {
		t.Fatal(err)
	}
	if want := defaultConfig(); c.Listen != want.Listen || c.Defaults != want.Defaults || !reflect.DeepEqual(c.History, want.History) || !reflect.DeepEqual(c.Costs, want.Costs) {
		t.Errorf("the example's values are not the defaults it says they are:\n%+v\n%+v", c, want)
	}
}
