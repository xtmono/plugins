# vdn-dhcp plugin

## Overview

With vdn-dhcp plugin the containers can get an IP allocated by a VDN vDHCP already running on SDN controller.
Because a DHCP lease must be periodically renewed for the duration of container lifetime, container orchestrator have to renew dhcp for container.

## Example configurations

```json
{
	"ipam": {
		"type": "vdn-dhcp",
		"servers": [
			{
				"url": "http://localhost:8181/vdn/v1/vdhcp/dhcp/",
				"user": "onos",
				"password": "rocks"
			}
		]
	}
}
```

```json
{
    "ips": [
        {
            "version": "4",
            "address": "172.16.1.10/24",
            "gateway": "172.16.1.1"
        }
    ],
    "dns": {
    	"nameservers": [
    		"164.124.101.2",
    		"203.248.252.2"
    	]
    }
}
```

## Network configuration reference

* `type` (string, required): "vdn-dhcp".
* `servers` (array, required): "dhcp server list".
* `url` (string, required): "rest server url".
* `user` (string): "user for rest connection".
* `password` (string): "password for rest connection".
