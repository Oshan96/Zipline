# Zipline's .rpm, for Fedora, openSUSE and their relatives. packaging/package.sh builds it from the same
# files as the .deb, already laid out in a staging folder: rpmbuild only packs them. (rpm expands macros
# even in comments, so none are named here.)
Name:           zipline
Version:        %{zl_version}
Release:        1
Summary:        Fast download manager with multi-connection downloads
License:        MIT
URL:            https://github.com/Oshan96/Zipline
Recommends:     xdg-utils
Recommends:     libsecret
AutoReqProv:    no

# a Go program built beforehand: nothing to compile, strip or split into a debug package
%global debug_package %{nil}
%global _build_id_links none
%global __os_install_post %{nil}

%description
Zipline downloads files over many connections at once and resumes them
after a lost connection or a restart. It also handles torrents and magnet
links, HLS and DASH video streams, metalinks with mirrors, FTP, and share
links from cloud services. Its window opens in Chrome, Edge, Brave or
Chromium, and a browser extension hands it the browser's downloads.

%install
cp -a %{stage}/. %{buildroot}/
install -Dm644 %{license_file} %{buildroot}/usr/share/licenses/zipline/LICENSE

%files
/usr/bin/zipline
/usr/share/applications/zipline.desktop
/usr/share/icons/hicolor/*/apps/zipline.*
/usr/share/zipline
%doc /usr/share/doc/zipline
%dir /usr/share/licenses/zipline
%license /usr/share/licenses/zipline/LICENSE
