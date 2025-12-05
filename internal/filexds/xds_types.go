package filexds

//go:generate sh -c "echo 'package filexds' > xds_types.gen.go"
//go:generate sh -c "echo 'import (' >> xds_types.gen.go"
//go:generate sh -c "go list github.com/envoyproxy/go-control-plane/... | grep 'v3' | grep -v /pkg/ | xargs -I{} echo '\t_ \"{}\"' >> xds_types.gen.go"
//go:generate sh -c "echo ')' >> xds_types.gen.go"
