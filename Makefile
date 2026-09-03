.PHONY: build web web-dev clean install-hooks

# Build everything: React app + Go binary with embedded assets.
# -p 2: same Windows commit-limit OOM mitigation as .githooks/pre-push
# (627219bf) and every CI test step in build.yml — full default package
# build parallelism has been a proven trigger on this machine.
build: web
	go build -p 2 -o rush .

# Build only the React app into web/dist/.
web:
	cd web && npm install && npm run build

# Start React dev server (pair with: rush web --port 3030 --no-open).
web-dev:
	cd web && npm install && npm run dev

clean:
	rm -rf web/dist web/node_modules rush

# Point git at the versioned hooks in .githooks/ (run once per clone).
install-hooks:
	git config core.hooksPath .githooks
	@echo "git hooks installed: core.hooksPath=.githooks"
