module shell

go 1.13

require (
	github.com/gentlemanautomaton/cmdline v0.0.0-20190611233644-681aa5e68f1c
	github.com/pterm/pterm v0.12.8
	gopkg.in/yaml.v2 v2.3.0
)

replace gopkg.in/yaml.v2 v2.3.0 => ./yaml.v2@v2.3.0/
