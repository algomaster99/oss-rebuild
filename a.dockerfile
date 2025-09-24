#syntax=docker/dockerfile:1.10
FROM docker.io/library/debian:trixie-20250203-slim
RUN <<'EOF'
 set -eux
 apt update
 apt install -y git wget maven
EOF
RUN <<'EOF'
 set -eux
 mkdir -p /src && cd /src
 git clone https://github.com/westwong/westdao .
 git checkout --force '451a7cea9df7c9ff4e45b684577a7c9d3cd10bb6'
 mkdir -p /opt/jdk
 wget -q -O - "https://download.java.net/java/ga/jdk11/openjdk-11_linux-x64_bin.tar.gz" | tar -xzf - --strip-components=1 -C /opt/jdk
EOF
RUN cat <<'EOF' >/build
 set -eux
 export JAVA_HOME=/opt/jdk
 export PATH=$JAVA_HOME/bin:$PATH
 mvn clean package -DskipTests --batch-mode -f WestDaoCore -Dmaven.javadoc.skip=true
 chmod +444 /src/WestDaoCore/target/westdao-core-1.2.8.jar
 mkdir -p /out && cp /src/WestDaoCore/target/westdao-core-1.2.8.jar /out/
EOF
WORKDIR "/src"
ENTRYPOINT ["/bin/sh","/build"]
