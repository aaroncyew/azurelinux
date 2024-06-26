ARG baseimage
FROM ${baseimage}

RUN tdnf update -y && \
   tdnf install -y dnf dnf-plugins-core

# Download the RPMs needed by the following tests:
# - TestCustomizeImageAddPackage
RUN dnf download -y --resolve --alldeps --destdir /downloadedrpms \
    jq \
    golang
