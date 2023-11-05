all:
	go build ./cmd/vhost
	go build ./cmd/vrouter

run: all
	util/vnet_run --host vhost --router reference/vrouter r1h2-test/

clean:
	rm -fv vhost vrouter