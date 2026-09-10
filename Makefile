
# IMGPKG_USERNAME / IMGPKG_PASSWORD must already be exported in your shell before running `make release`.

release: build-controller release-controller
	# @printf "#@data/values\n---\nversion: $(VERSION)\n" > config/values.yml

	cp controller/manifests/crd.yml config/crd.yml
	kctrl package release -y -v ${VERSION} --debug

	cp carvel-artifacts/packages/label-policy.field.vmware.com/metadata.yml ./label-policy.yml
	echo "\n---" >> ./label-policy.yml
	cat carvel-artifacts/packages/label-policy.field.vmware.com/package.yml >> ./label-policy.yml

build-controller:
	docker build -f controller/Dockerfile -t ghcr.io/plameniliev/label-policy-supervisor-service/controller:${VERSION} controller/.
release-controller:
	docker push ghcr.io/plameniliev/label-policy-supervisor-service/controller:${VERSION}
