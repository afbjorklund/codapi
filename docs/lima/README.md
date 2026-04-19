# codapi-lima

CNCF Lima - Linux Machines.
See <https://lima-vm.io/>

You can run the Codapi sandboxes in containers, with one of
the provided Lima container runtimes (containerd/Docker/Podman).

See <https://lima-vm.io/docs/examples/containers/>

Or you can run a new dedicated Lima instance only for Codapi,
including both the server and the sandboxes in a separate VM.

This way, **no** files from the host are shared with it.

This is a mix of the Debian and Docker (rootful) templates,
with the Codapi provisioning from install.md added on top.

It will run the codapi server in a virtual machine instance,
and export codapi on port 1313 for usage on the host system.

To start a new instance:
`limactl start codapi.yaml`

When running in containers, it will share the resources with
other containers. But the virtual machine will be dedicated.

If you want to specify the amount of resources for the new
instance, you can add some flags like `--cpus 2 --memory 2`.

See https://lima-vm.io/docs/config/ for all the details.

To access the codapi instance:
`limactl shell codapi`

Then to access the Codapi installation, from the shell:

```shell
sudo su - codapi
cd /opt/codapi
```
