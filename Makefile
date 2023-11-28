all:
	go build ./cmd/vhost
	go build ./cmd/vrouter

run: all
	util/vnet_run --host vhost --router reference/vrouter tests/linear-r1h2/

clean:
	rm -fv vhost vrouter