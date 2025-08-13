#!/bin/bash

# JDK URL Status Checker
# Usage: ./check_jdk_urls.sh

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# JDK URLs to check
declare -A jdk_urls=(
    ["8u451"]="https://javadl.oracle.com/webapps/download/GetFile/1.8.0_451-b10/8a1589aa0fe24566b4337beee47c2d29/linux-i586/jdk-8u451-linux-x64.tar.gz"
    ["8u441"]="https://javadl.oracle.com/webapps/download/GetFile/1.8.0_441-b07/7ed26d28139143f38c58992680c214a5/linux-i586/jdk-8u441-linux-x64.tar.gz"
    ["8u431"]="https://javadl.oracle.com/webapps/download/GetFile/1.8.0_431-b10/0d8f12bc927a4e2c9f8568ca567db4ee/linux-i586/jdk-8u431-linux-x64.tar.gz"
    ["8u421"]="https://javadl.oracle.com/webapps/download/GetFile/1.8.0_421-b09/d8aa705069af427f9b83e66b34f5e380/linux-i586/jdk-8u421-linux-x64.tar.gz"
    ["8u411"]="https://javadl.oracle.com/webapps/download/GetFile/1.8.0_411-b09/43d62d619be4e416215729597d70b8ac/linux-i586/jdk-8u411-linux-x64.tar.gz"
    ["8u401"]="https://javadl.oracle.com/webapps/download/GetFile/1.8.0_401-b10/4d245f941845490c91360409ecffb3b4/linux-i586/jdk-8u401-linux-x64.tar.gz"
    ["8u391"]="https://javadl.oracle.com/webapps/download/GetFile/1.8.0_391-b13/b291ca3e0c8548b5a51d5a5f50063037/linux-i586/jdk-8u391-linux-x64.tar.gz"
    ["8u381"]="https://javadl.oracle.com/webapps/download/GetFile/1.8.0_381-b09/8c876547113c4e4aab3c868e9e0ec572/linux-i586/jdk-8u381-linux-x64.tar.gz"
    ["8u371"]="https://javadl.oracle.com/webapps/download/GetFile/1.8.0_371-b11/ce59cff5c23f4e2eaf4e778a117d4c5b/linux-i586/jdk-8u371-linux-x64.tar.gz"
    ["8u361"]="https://javadl.oracle.com/webapps/download/GetFile/1.8.0_361-b09/0ae14417abb444ebb02b9815e2103550/linux-i586/jdk-8u361-linux-x64.tar.gz"
    ["8u351"]="https://javadl.oracle.com/webapps/download/GetFile/1.8.0_351-b10/10e8cce67c7843478f41411b7003171c/linux-i586/jdk-8u351-linux-x64.tar.gz"
    ["8u341"]="https://javadl.oracle.com/webapps/download/GetFile/1.8.0_341-b10/424b9da4b48848379167015dcc250d8d/linux-i586/jdk-8u341-linux-x64.tar.gz"
    ["8u333"]="https://javadl.oracle.com/webapps/download/GetFile/1.8.0_333-b02/2dee051a5d0647d5be72a7c0abff270e/linux-i586/jdk-8u333-linux-x64.tar.gz"
    ["8u331"]="https://javadl.oracle.com/webapps/download/GetFile/1.8.0_331-b09/165374ff4ea84ef0bbd821706e29b123/linux-i586/jdk-8u331-linux-x64.tar.gz"
    ["8u321"]="https://javadl.oracle.com/webapps/download/GetFile/1.8.0_321-b07/df5ad55fdd604472a86a45a217032c7d/linux-i586/jdk-8u321-linux-x64.tar.gz"
    ["8u311"]="https://javadl.oracle.com/webapps/download/GetFile/1.8.0_311-b11/4d5417147a92418ea8b615e228bb6935/linux-i586/jdk-8u311-linux-x64.tar.gz"
    ["8u301"]="https://javadl.oracle.com/webapps/download/GetFile/1.8.0_301-b09/d3c52aa6bfa54d3ca74e617f18309292/linux-i586/jdk-8u301-linux-x64.tar.gz"
    ["8u291"]="https://javadl.oracle.com/webapps/download/GetFile/1.8.0_291-b10/d7fc238d0cbf4b0dac67be84580cfb4b/linux-i586/jdk-8u291-linux-x64.tar.gz"
    ["8u281"]="https://javadl.oracle.com/webapps/download/GetFile/1.8.0_281-b09/89d678f2be164786b292527658ca1605/linux-i586/jdk-8u281-linux-x64.tar.gz"
    ["8u271"]="https://javadl.oracle.com/webapps/download/GetFile/1.8.0_271-b09/61ae65e088624f5aaa0b1d2d801acb16/linux-i586/jdk-8u271-linux-x64.tar.gz"
    ["8u261"]="https://javadl.oracle.com/webapps/download/GetFile/1.8.0_261-b12/a4634525489241b9a9e1aa73d9e118e6/linux-i586/jdk-8u261-linux-x64.tar.gz"
    ["8u251"]="https://javadl.oracle.com/webapps/download/GetFile/1.8.0_251-b08/3d5a2bb8f8d4428bbe94aed7ec7ae784/linux-i586/jdk-8u251-linux-x64.tar.gz"
    ["8u241"]="https://javadl.oracle.com/webapps/download/GetFile/1.8.0_241-b07/1f5b5a70bf22433b84d0e960903adac8/linux-i586/jdk-8u241-linux-x64.tar.gz"
)

# Check if curl is available
if ! command -v curl &> /dev/null; then
    echo -e "${RED}Error: curl is not installed. Please install curl to run this script.${NC}"
    exit 1
fi

# Initialize counters
success_count=0
failed_count=0
total_count=${#jdk_urls[@]}

# Arrays to store results
successful_versions=()
failed_versions=()

echo -e "${BLUE}JDK URL Status Checker${NC}"
echo "======================="
echo -e "Checking ${total_count} URLs...\n"

# Sort versions numerically (highest to lowest)
sorted_versions=($(printf '%s\n' "${!jdk_urls[@]}" | sed 's/8u//' | sort -nr | sed 's/^/8u/'))

count=0
for version in "${sorted_versions[@]}"; do
    count=$((count + 1))
    url="${jdk_urls[$version]}"
    
    echo -n -e "[${count}/${total_count}] Checking ${YELLOW}${version}${NC}... "
    
    # Use curl to check HTTP status (HEAD request, follow redirects, timeout 10s)
    http_status=$(curl -s -I -L --connect-timeout 10 --max-time 30 -o /dev/null -w "%{http_code}" "$url" 2>/dev/null)
    
    if [ "$http_status" = "200" ]; then
        echo -e "${GREEN}✅ 200 OK${NC}"
        success_count=$((success_count + 1))
        successful_versions+=("$version")
    else
        if [ -z "$http_status" ] || [ "$http_status" = "000" ]; then
            echo -e "${RED}❌ Connection Error${NC}"
            failed_versions+=("$version (Connection Error)")
        else
            echo -e "${RED}❌ $http_status${NC}"
            failed_versions+=("$version ($http_status)")
        fi
        failed_count=$((failed_count + 1))
    fi
    
    # Small delay to be respectful to the server
    sleep 0.5
done

# Summary
echo ""
echo "======================================"
echo -e "${BLUE}SUMMARY:${NC}"
echo -e "${GREEN}✅ Successful: $success_count${NC}"
echo -e "${RED}❌ Failed: $failed_count${NC}"

if [ ${#successful_versions[@]} -gt 0 ]; then
    echo ""
    echo -e "${GREEN}✅ Working URLs:${NC}"
    for version in "${successful_versions[@]}"; do
        echo "  - $version"
    done
fi

if [ ${#failed_versions[@]} -gt 0 ]; then
    echo ""
    echo -e "${RED}❌ Failed URLs:${NC}"
    for version_info in "${failed_versions[@]}"; do
        echo "  - $version_info"
    done
fi

echo ""
if [ $failed_count -eq 0 ]; then
    echo -e "${GREEN}All URLs return 200: YES ✅${NC}"
    exit 0
else
    echo -e "${RED}All URLs return 200: NO ❌${NC}"
    exit 1
fi