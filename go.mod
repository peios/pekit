module github.com/peios/pekit

go 1.26.2

require (
	github.com/BurntSushi/toml v1.6.0
	github.com/bmatcuk/doublestar/v4 v4.10.0
	github.com/klauspost/compress v1.18.6
	github.com/peios/peipkg v0.0.0
)

require (
	github.com/peios/libp-go v0.8.0 // indirect
	github.com/peios/pkm/uapi/go v0.20.0 // indirect
)

replace github.com/peios/peipkg => ../peipkg
