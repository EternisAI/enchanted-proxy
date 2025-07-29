# Cryptographic Attestation

## Introduction

This document describes the way to verify the security guarantees provided by the code in this repository.

The code built from this repository is expected to run in an isolated, trusted execution environment (TEE) implemented by an [AWS Nitro Enclave](https://docs.aws.amazon.com/enclaves/latest/user/nitro-enclave.html). The Nitro Hypervisor running these enclaves provides a cryptographically signed attestation document containing the measurements unique to the enclave, called PCRs, and other provenance information which can be used to verify that the code serving the requests is the genuine, unmodified code built from this repository.

## Retrieving the Attestation Document

The attestation document can be retrieved by accessing the `/-/attestation` URL at the domain where the code built from this repository is running, for example: https://proxy-api.enchanted.freysa.ai/-/attestation

It is highly recommended to supply a random nonce value, which should be a hex-encoded number, as a query parameter to the URL above, to prevent replay attacks and to ensure that the response you receive corresponds to your unique request. A suitable nonce can be obtained, for example, by running the following command: `openssl rand -hex 32` The resulting URL may look like: https://proxy-api.enchanted.freysa.ai/-/attestation?nonce=e24840ca365d68d600fb2518c63116457f20ac8e44152aaf3d9ea18b93271a11

The endpoint returns a base64-encoded text of the original attestation document from the Nitro Hypervisor, which is a CBOR-encoded binary. For attestation purposes it may be more convenient to directly retrieve the original binary rather than decode the text output (which can be done by supplying the output to `base64 -d` command). To request the binary representation, add `cbor=true` query parameter to the URL, like this: https://proxy-api.enchanted.freysa.ai/-/attestation?cbor=true&nonce=e24840ca365d68d600fb2518c63116457f20ac8e44152aaf3d9ea18b93271a11

## Verifying the Attestation Document

At the moment there is no simple tool to verify all data in the retrieved attestation document in one go, but the following instructions will help to do this manually step by step.

### Verifying the Validity of the Document

First of all, use https://edgebit.io/attestation/ web page, courtesy of EdgeBit, a company developing the [Enclaver](https://enclaver.io) tool which we leverage to run our workloads in Nitro enclaves. You can upload the binary attestation document retrieved as described above to this page and obtain the initial validation result which demonstrates that the workload which produced the document is running in a genuine AWS Nitro enclave.

After that you should verify that the nonce displayed in the validation results on the page is the same that you specified in the document request.

### Decoding User Data from the Attestation Document

The next step is to decode the user data provided in the attestation document. You can use the following command, substituting with your own user data value from the validation result:

```sh
echo 'eyJlbmNsYXZlIjp7ImJ1aWxkX2lkIjoiMzkzYjZhNzFlMzY5MTUzMGQyNDUxNWFlNmMwNWJkMzFmZDBlY2Y2NyIsImJ1aWxkX3ZlcnNpb24iOiJzaGEtMzkzYjZhNyIsInByb3ZlbmFuY2VfcGF0aCI6ImVuY2hhbnRlZC1wcm94eS9zZXJ2ZXIifSwidGxzIjp7ImNlcnRpZmljYXRlIjoiOGIzYmUwMDA3ZTIwMGQzNTY5MmUyY2YzMDg5MGRkNzNiM2U4Y2FlMjEzNTFkNzJkNjViYzc2YjMyYzNkYjQwMCIsInB1YmxpY19rZXkiOiI0ZWYwNzUzZDI2MjYxYjcyM2JlOTcwM2E1YjJiYTUzNWQ1ZjVlNzhkMWEzYjJjZWU3MWM1N2Q0NWNlNmNlZGI4In19' \
| base64 -d | jq
```

You should receive an output similar to the following one:

```json
{
  "enclave": {
    "build_id": "393b6a71e3691530d24515ae6c05bd31fd0ecf67",
    "build_version": "sha-393b6a7",
    "provenance_path": "enchanted-proxy/server"
  },
  "tls": {
    "certificate": "8b3be0007e200d35692e2cf30890dd73b3e8cae21351d72d65bc76b32c3db400",
    "public_key": "4ef0753d26261b723be9703a5b2ba535d5f5e78d1a3b2cee71c57d45ce6cedb8"
  }
}
```

### Verifying the PCR Values of the Enclave

The `enclave` part of this output contains information about the build of the code running in the enclave. It can be used to verify the PCR values included in the attestation document, as they are unique to every build and are published alongside the built image.

To obtain the original PCR values of the build, visit the following URL, substituting with the values of `provenance_path` and `build_id` and from the `enclave` section of your decoded user data:

https://provenance.eternis.ai/enchanted-proxy/server/39/3b/6a/393b6a71e3691530d24515ae6c05bd31fd0ecf67

Please note that the `provenance_path` value should form the URL prefix after https://provenance.eternis.ai, and the path following that prefix consists of the first two characters (#1 and #2) of the `build_id` value, the next two characters (#3 and #4) and the next two characters (#5 and #6) of the `build_id` value as subdirectories, followed by the entire `build_id` value.

This URL should produce a JSON document containing PCR measurements, similar to the following one:

```json
{
  "Measurements": {
    "PCR0": "ba2b9b18609856341b1910b044d50133785e01ca84d3530b7b16cce1f15ef87f24a990fdec8a016da3f0570e6fb04efd",
    "PCR1": "4b4d5b3661b3efc12920900c80e126e4ce783c522de6c02a2a5bf7af3a2b9327b86776f188e4be1c1c404a129dbda493",
    "PCR2": "044f15f6f448a959fd281943b1926ec517529846f2dd4fece6356682df4f0c2a0c76eb4762c416fcb8a6cc0ec38c7d0f"
  }
}
```

Now you can compare the values provided in this document with the PCR0-PCR2 values provided in the result of validation of the attestation document. If they match, the enclave is running the original genuine build. If there is a discrepancy or the file is not found, the verification is considered failed.

### Verifying the Build Running in the Enclave

Measurements for builds are published to https://provenance.eternis.ai during the build process, but these files don't provide any extra verification of their genuineness and you have to include https://provenance.eternis.ai in the trusted computing base if you only rely on it for verification.

To protect yourself from cases when the contents of https://provenance.eternis.ai may be tampered with, the second layer of protection can be achieved by verifying the code build running in the enclave itself, as it is published to a public registry alongside the provenance measurements during the build process.

You should have Cosign CLI [installed](https://github.com/sigstore/cosign?tab=readme-ov-file#installation) to perform this verification.

Then you should issue the following command (substituting with values from your decoded user data):

```sh
cosign verify public.ecr.aws/f5z1z8p0/enchanted-proxy/server:sha-393b6a7 \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity 'https://github.com/EternisAI/enchanted-proxy/.github/workflows/ci-server.yaml@refs/heads/main'
```

Where, in the commands above,

* `public.ecr.aws/f5z1z8p0` is the URL of the AWS Public ECR repository used to publish builds of code from this repository during the CI workflow,
* `enchanted-proxy/server` is the `provenance_path` value from your decoded user data,
* `sha-15c7db1` is the value of `build_version` from your decoded user data,
* https://token.actions.githubusercontent.com is the [OIDC issuer](https://docs.github.com/en/actions/concepts/security/openid-connect) URL for Github Actions jobs,
* `https://github.com/EternisAI/enchanted-proxy` is the path of the current Github repostory,
* `.github/workflows/ci-server.yaml` is the path of the build CI workflow in the current repository.

If the `cosign` command exited successfully, it means that:

* the code running in the enclave is deployed from the same public image you provided as an argument to the `cosign verify` command;
* the image has been built by the particular CI workflow from this repository, executed by Github Actions;
* the image has been built from the tag or commit specified in the attestation document;
* the authenticity of the image is signed by Github, and verified and stored in a public transparency log (Rekor) maintained by [Sigstore](https://docs.sigstore.dev/about/overview/).

Moreover, the output of a successful verification by the `cosign verify` command shown above is a JSON manifest containing provenance data, including the PCR measurements produced during the build which were originally uploaded to https://provenance.eternis.ai. You can extract them by supplying the output of the `cosign verify` command to `jq` like this:

```sh
cosign verify public.ecr.aws/f5z1z8p0/enchanted-proxy/server:sha-393b6a7 \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity 'https://github.com/EternisAI/enchanted-proxy/.github/workflows/ci-server.yaml@refs/heads/main' \
| jq '{"Measurements": {"PCR0": .[0].optional.awsNitroEnclavePCR0, "PCR1": .[0].optional.awsNitroEnclavePCR1, "PCR2": .[0].optional.awsNitroEnclavePCR2}}'
```

The contents of this output should be the same as the file retrieved from https://provenance.eternis.ai, and the PCR values in it should be the same as shown in the validation result of the original attestation document.

### Verifying the Security of the TLS Connection

The `tls` section of the decoded user data contains SHA-256 fingerprints of DER-encoded public key and certificate of the TLS keypair used to secure the communication between the user and the enclave. The `Public Key` field of the validated attestation document also contains a DER-encoded data of the same public key.

To verify the `Public Key` field against the fingerprint provided in the `tls` section, you should re-encode it to a DER public key container and obtain its SHA-256 hash by running the following command (substitute with the public key data from your validated attestation document):

```sh
echo 'MIIBCgKCAQEAxF8rvtBBpV91Uok8fgmdW5dbf0cwrFQz0RbXdmh9b5WMANXC4Gpz/CfTRfMD0xtAkWLqb8Y/LT42WoUZde8CdyQ/jVpHgeDC9Ba3u11/tfr7oGCQLILvhP7ZGmmDbKyzjCAyNuCTCfAHqyOF240ILtGALnuhmdZywgYsunq7FK7URXymG56LVFk3XyJxj4LRQQrNQ6eahkkSWyUNAjgpMO1Cr47GPNqNKy3NDsM7G7fWp/snzLGfn0tTFsPz6y8sRl+aRqACYdmHqLZ5ckO91ecOJG0sS10OOTSGEqIoxCD+RIjKBN0lvr5/MBc+NlEgeHKdp5Fo1KvVk56iBy2I5wIDAQAB' \
| base64 -d | openssl rsa -pubin -outform DER | sha256sum
```

The hash value produced by `sha256sum` should be the same as the `public_key` value from the `tls` section of the decoded user data.

The next step is to verify the fingerprints in your browser. Navigate to the attestation endpoint (or any other endpoint served by the same domain/code) in the browser and open the parameters of the TLS certificate (in Chrome this can be done by clicking a round button with two jumpers on the left of the URL in the toolbar, then clicking "Connection is secure", then clicking "Certificate is valid"). The SHA-256 fingerprints of the certificate and the public key displayed by the browser should match the corresponding values of `certificate` and `public_key` keys from the `tls` section of the decoded user data.

If these values match, it means the data transferred to the enclave is end-to-end encrypted, with decryption happening only inside the enclave, where the data is protected by hardware from being tampered with or intercepted.

## Trusted Computing Base

The instructions above allow verifying that the code serving the requests is genuine and unmodified and that the data is secure in transit to it, but the build of the code includes multiple open source components, and verification of each of these components during the build process is not always possible or practical. As such, the following components are used in the image build and should be considered trusted in the form they are used as described below:

* Golang modules used in the source code.
* Github Actions used in the CI pipelines - all of them are pinned to a specific git commit SHA.<br/><br/>
* Official [Golang Alpine-based](https://hub.docker.com/_/golang) docker images - used to build the source code.
* Official [Prometheus Node Exporter](https://github.com/prometheus/node_exporter) images published to [quay.io](https://quay.io/prometheus/node-exporter).
* Official [Rust Alpine-based](https://hub.docker.com/_/rust) docker images - used to build `procfusion` (see below).
	* The following extra APK packages installed during the build:
		* `git`
		* `musl-dev`
* Official [Envoy proxy](https://hub.docker.com/r/envoyproxy/envoy) docker images.
* [Attestation Proxy](https://github.com/EternisAI/attestation-proxy) docker images published to an AWS Public ECR [repository](https://gallery.ecr.aws/f5z1z8p0/attestation-proxy/server).

Note: the images listed above are pinned in the Dockerfile used for the build to SHA256 digests of the specific tags used. The Attestation Proxy image is additionally verified during the build using `cosign verify`.

* [procfusion](https://github.com/linkdd/procfusion) - built from the source code pinned to a specific git commit SHA.
* Ubuntu 22.04 LTS (Jammy Jellyfish) - the base for the Envoy proxy image, which is used as the base for the build image.
	* The following extra APT packages installed during the build:
		* `ca-certificates`
		* `dnsmasq`
		* `iproute2`
		* `netcat-openbsd`

The actual docker image to run the enclave is built by a [modified version](https://github.com/EternisAI/enclaver) of the [Enclaver](https://enclaver.io) tool. Three different images built from the modified code base are used in the build process. All of these images are pinned to SHA256 digests of the specific tags used, and verified by `cosign verify` during the build process:

* [Enclaver CLI tool](https://gallery.ecr.aws/f5z1z8p0/enclaver) docker image
* [Enclaver TEE runner](https://gallery.ecr.aws/f5z1z8p0/enclaver-wrapper-base) docker image
* [Enclaver TEE supervisor](https://gallery.ecr.aws/f5z1z8p0/odyn) ("Odyn") docker image
