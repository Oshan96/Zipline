package main

// appVersion is Zipline's version. The source holds the major and minor parts: raise it (1.1, 2.0) to
// start a new series. A release build gets the full version (1.0.3) from packaging/package.sh, through
// -ldflags "-X main.appVersion=…", and the release workflow picks the patch number.
var appVersion = "1.0"
