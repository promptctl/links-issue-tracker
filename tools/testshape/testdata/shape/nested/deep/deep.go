// Package deep has an initializer the fixture classification charges fs.
// No module package imports it: only nested/front does.
package deep

// Home is set by the initializer.
var Home = home()

func home() string { return "/home" }
