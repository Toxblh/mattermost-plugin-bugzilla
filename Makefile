.PHONY: test dist

test:
	go test -race ./server
	npm ci --prefix webapp
	npm run typecheck --prefix webapp
	npm test --prefix webapp

dist:
	./scripts/build.sh
