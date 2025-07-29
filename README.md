# Enchanted TEE Proxy

This project is a secure proxy designed to run inside a Trusted Execution Environment (TEE), specifically leveraging AWS Nitro Enclaves, for the [Enchanted](https://github.com/eternisai/enchanted-twin) Personal AI app. AWS Nitro Enclaves provide hardware-isolated compute environments that ensure sensitive data and operations remain protected, even from users with root access to the host system. By utilizing Nitro Enclaves, the proxy can securely forward and mediate API requests between the Enchanted app and external AI services, guaranteeing that sensitive data is never exposed outside the enclave boundary.

Operating within an AWS Nitro Enclave, the proxy offers strong guarantees of data privacy and integrity. User information is protected from unauthorized access, even if the underlying infrastructure or host operating system is compromised. All requests from the Enchanted app are routed through this enclave-based proxy, which enforces authentication, logging, and policy controls before securely relaying them to their intended destinations.

This architecture, built on AWS Nitro Enclaves, is designed to give users confidence that their personal data and AI interactions remain confidential and protected at all times, leveraging the latest advancements in cloud hardware security.

You can read instructions on verifying the security guarantees provided by the live AWS Nitro Enclave deployments of this project in the [following document](docs/attestation.md).

## License

BSD
