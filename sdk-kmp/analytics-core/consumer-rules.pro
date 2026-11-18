# izlek analytics-core consumer rules.
#
# The SDK uses no reflection, no serialization frameworks and no JNI, so it needs no keep rules:
# R8 may shrink, rename and inline everything. This file exists so that future rules have a home
# and so that applications can see that nothing is kept on purpose.
