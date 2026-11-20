dep-gawk:
	@ if [ -z "$$(command -v gawk)" ]; then \
		if [ -x /usr/bin/apt-get ]; then apt-get -qq install -y gawk; \
		else echo "GNU Awk required"; exit 1; fi; \
	fi

help: dep-gawk
	@cat $(MAKEFILE_LIST) | \
		grep -E '^# ~~~ .*? [~]+$$|^[a-zA-Z0-9_-]+:.*?## .*$$' | \
		gawk '{if ( $$1=="#" ) { \
			match($$0, /^# ~~~ (.+?) [~]+$$/, a);\
			{print "\n", a[1], ""}\
		} else { \
			match($$0, /^([0-9a-zA-Z_-]+):.*?## (.*)$$/, a); \
			{printf "  - \033[32m%-20s\033[0m %s\n",   a[1], a[2]} \
 		}}'
	@echo ""
