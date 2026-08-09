// Package extensionregistry contains QED's generated self-exec Extension catalog
package extensionregistry

//go:generate go run ../../cmd/qed-extension-gen --lock ../../extensions.lock --output registry_gen.go --package extensionregistry --variable Catalog
