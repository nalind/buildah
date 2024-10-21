#!/bin/bash
packages=$(rpm -qa --qf '%{name}\n' | sort)
npkg=0
echo '{'
echo '  "layers": ['
for pkg in $packages ; do
	if test $((npkg++)) -gt 0 ; then
		echo ','
	fi
	echo '    ['
	rpm -q --qf '[      "%{filenames}"\n]' $pkg | sed -r -e 's:(.*):\1,:' -e '$s:,$::'
	echo '    ]'
done
echo '  ]'
echo '}'
