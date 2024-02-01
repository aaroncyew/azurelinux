%global gem_name ffi
Summary:        Ruby FFI
Name:           rubygem-ffi
Version:        1.16.3
Release:        1%{?dist}
License:        BSD
Vendor:         Microsoft Corporation
Distribution:   Mariner
Group:          Development/Languages
URL:            https://github.com/ffi/ffi
Source0:        https://github.com/ffi/ffi/archive/refs/tags/v%{version}.tar.gz#/%{gem_name}-%{version}.tar.gz
BuildRequires:  git
BuildRequires:  ruby
Provides:       rubygem(%{gem_name}) = %{version}-%{release}

%description
Ruby-FFI is a gem for programmatically loading dynamically-linked native libraries,
binding functions within them, and calling those functions from Ruby code. Moreover,
a Ruby-FFI extension works without changes on CRuby (MRI), JRuby, Rubinius and TruffleRuby.

%prep
%autosetup -n %{gem_name}-%{version}

%build
gem build %{gem_name}

%install
gem install -V --local --force --install-dir %{buildroot}/%{gemdir} %{gem_name}-%{version}.gem

%files
%defattr(-,root,root,-)
%license LICENSE
%{gemdir}

%changelog
* Wed Jan 31 2024 Pawel Winogrodzki <pawelwi@microsoft.com> - 1.16.3-1
- Upgrading to the latest version.

* Wed Sep 20 2023 Jon Slobodzian <joslobo@microsoft.com> - 1.15.5-2
- Recompile with stack-protection fixed gcc version (CVE-2023-4039)

* Wed Jun 22 2022 Neha Agarwal <nehaagarwal@microsoft.com> - 1.15.5-1
- Update to v1.15.5.
- Build from .tar.gz source.

* Wed Jan 06 2021 Henry Li <lihl@microsoft.com> - 1.13.1-1
- License verified
- Original version for CBL-Mariner
