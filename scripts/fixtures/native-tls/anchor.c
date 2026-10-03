#include <CoreFoundation/CoreFoundation.h>
#include <Security/Security.h>
#include <mach-o/dyld.h>
#include <limits.h>
#include <stdbool.h>
#include <stdlib.h>
#include <string.h>
#include "fixture-anchor.h"

static int fixture_descendant(void) {
    char executable[PATH_MAX];
    char resolved[PATH_MAX];
    uint32_t size = sizeof(executable);
    if (_NSGetExecutablePath(executable, &size) != 0 || realpath(executable, resolved) == NULL)
        return 0;
    size_t length = strlen(fixture_root);
    return strncmp(resolved, fixture_root, length) == 0 && resolved[length] == '/';
}

static OSStatus fixture_create_trust(CFTypeRef certificates, CFTypeRef policies, SecTrustRef *trust) {
    OSStatus status = SecTrustCreateWithCertificates(certificates, policies, trust);
    if (status != errSecSuccess || !fixture_descendant())
        return status;
    CFDataRef data = CFDataCreate(NULL, fixture_ca_der, sizeof(fixture_ca_der));
    if (data == NULL)
        return status;
    SecCertificateRef anchor = SecCertificateCreateWithData(NULL, data);
    CFRelease(data);
    if (anchor == NULL)
        return status;
    const void *values[] = {anchor};
    CFArrayRef anchors = CFArrayCreate(NULL, values, 1, &kCFTypeArrayCallBacks);
    if (anchors != NULL) {
        // Only this object's anchor changes; SecTrust still verifies the chain and SSL policy.
        if (SecTrustSetAnchorCertificates(*trust, anchors) != errSecSuccess ||
            SecTrustSetAnchorCertificatesOnly(*trust, true) != errSecSuccess)
            SecTrustSetAnchorCertificates(*trust, NULL);
        CFRelease(anchors);
    }
    CFRelease(anchor);
    return status;
}

__attribute__((used)) static const struct {
    const void *replacement;
    const void *original;
} fixture_interpose __attribute__((section("__DATA,__interpose"))) = {
    (const void *)&fixture_create_trust,
    (const void *)&SecTrustCreateWithCertificates,
};
